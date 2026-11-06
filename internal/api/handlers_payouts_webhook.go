package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// PayoutWebhookEnvelope represents the outer wrapper of a Cashfree Transfers V2 webhook event.
type PayoutWebhookEnvelope struct {
	Event          string          `json:"event"`
	Type           string          `json:"type"`
	EventTime      string          `json:"event_time"`
	Timestamp      string          `json:"timestamp"`
	Signature      string          `json:"signature"`
	EventSignature string          `json:"event_signature"`
	Data           json.RawMessage `json:"data"`
}

// PayoutWebhookData holds fields for individual or batch payout notifications.
type PayoutWebhookData struct {
	TransferID        string      `json:"transfer_id"`
	CFTransferID      string      `json:"cf_transfer_id"`
	BatchTransferID   string      `json:"batch_transfer_id"`
	CFBatchTransferID string      `json:"cf_batch_transfer_id"`
	Status            string      `json:"status"`
	StatusCode        string      `json:"status_code"`
	StatusDescription string      `json:"status_description"`
	TransferAmount    json.Number `json:"transfer_amount"`
	TransferUTR       *string     `json:"transfer_utr"`
	UTR               *string     `json:"utr"`
	Acknowledged      *int        `json:"acknowledged,omitempty"`
	AcknowledgedAt    *string     `json:"acknowledged_at"`
	SettledAt         *string     `json:"settled_at"`
}

// buildAllowedPayoutAuditPayload extracts allow-listed operational fields from the payout webhook
// for persistent audit storage. Sensitive beneficiary bank details, IFSC, and signature headers
// are strictly omitted to enforce data minimization.
func buildAllowedPayoutAuditPayload(env PayoutWebhookEnvelope, data PayoutWebhookData) []byte {
	evtType := strings.ToUpper(strings.TrimSpace(env.Event))
	if evtType == "" {
		evtType = strings.ToUpper(strings.TrimSpace(env.Type))
	}
	if evtType == "" {
		evtType = "PAYOUT"
	}

	dataMap := map[string]any{
		"transfer_id":          data.TransferID,
		"cf_transfer_id":       data.CFTransferID,
		"batch_transfer_id":    data.BatchTransferID,
		"cf_batch_transfer_id": data.CFBatchTransferID,
		"status":               data.Status,
		"status_code":          data.StatusCode,
		"status_description":   data.StatusDescription,
	}

	if data.TransferAmount.String() != "" {
		dataMap["transfer_amount"] = data.TransferAmount.String()
	}
	if data.UTR != nil {
		dataMap["utr"] = *data.UTR
	}
	if data.TransferUTR != nil {
		dataMap["transfer_utr"] = *data.TransferUTR
	}
	if data.Acknowledged != nil {
		dataMap["acknowledged"] = *data.Acknowledged
	}
	if data.AcknowledgedAt != nil {
		dataMap["acknowledged_at"] = *data.AcknowledgedAt
	}
	if data.SettledAt != nil {
		dataMap["settled_at"] = *data.SettledAt
	}

	auditMap := map[string]any{
		"event":      evtType,
		"event_time": env.EventTime,
		"data":       dataMap,
	}

	b, _ := json.Marshal(auditMap)
	return b
}

// verifyPayoutLowBalanceBodySignature verifies the legacy field-signature format
// Cashfree documents for its LOW_BALANCE_ALERT Dashboard test callback. The
// exception is intentionally restricted to flat scalar fields: accepting
// arbitrary nested values would make the signing representation ambiguous.
func verifyPayoutLowBalanceBodySignature(secret string, raw []byte, signature string) bool {
	if strings.TrimSpace(secret) == "" || strings.TrimSpace(signature) == "" {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || len(fields) == 0 {
		return false
	}
	keys := make([]string, 0, len(fields))
	values := make(map[string]string, len(fields))
	for key, encoded := range fields {
		lower := strings.ToLower(key)
		if lower == "signature" || lower == "event_signature" {
			continue
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(encoded))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			return false
		}
		switch v := value.(type) {
		case string:
			values[key] = v
		case json.Number:
			values[key] = v.String()
		case bool:
			values[key] = fmt.Sprint(v)
		case nil:
			values[key] = ""
		default:
			return false
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return false
	}
	sort.Strings(keys)
	var postData strings.Builder
	for _, key := range keys {
		if values[key] != "" {
			postData.WriteString(values[key])
		}
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(postData.String()))
	want := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(want), []byte(signature))
}

// CashfreePayoutWebhook handles POST /webhooks/cashfree/payouts.
func (h *Handlers) CashfreePayoutWebhook(c *gin.Context) {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxWebhookBody+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if len(raw) > maxWebhookBody {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "body too large"})
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))

	// Fail closed if payout webhook secret is unconfigured
	var candidateSecrets []string
	if len(h.CashfreePayoutWebhookSecrets) > 0 {
		for _, s := range h.CashfreePayoutWebhookSecrets {
			if strings.TrimSpace(s) != "" {
				candidateSecrets = append(candidateSecrets, strings.TrimSpace(s))
			}
		}
	}
	if strings.TrimSpace(h.CashfreePayoutWebhookSecret) != "" {
		trimmed := strings.TrimSpace(h.CashfreePayoutWebhookSecret)
		found := false
		for _, s := range candidateSecrets {
			if s == trimmed {
				found = true
				break
			}
		}
		if !found {
			candidateSecrets = append(candidateSecrets, trimmed)
		}
	}

	if len(candidateSecrets) == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "payout webhook secret not configured"})
		return
	}

	var env PayoutWebhookEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid webhook JSON"})
		return
	}
	evtTypePeek := strings.ToUpper(strings.TrimSpace(env.Event))
	if evtTypePeek == "" {
		evtTypePeek = strings.ToUpper(strings.TrimSpace(env.Type))
	}

	sig := strings.TrimSpace(c.GetHeader("x-webhook-signature"))
	ts := strings.TrimSpace(c.GetHeader("x-webhook-timestamp"))
	validSig := false
	usesBodySignature := false
	if sig != "" && ts != "" {
		for _, s := range candidateSecrets {
			if cashfree.VerifyWebhookHMAC(s, ts, string(raw), sig) {
				validSig = true
				break
			}
		}
	}

	// Cashfree's Dashboard "Test & Add" LOW_BALANCE_ALERT is the one documented
	// exception to the V2 header contract: it carries its signature and timestamp
	// in the JSON payload. Verify that event with the legacy Payouts field-signing
	// algorithm, and never let that exception authenticate transfer events.
	if !validSig && evtTypePeek == "LOW_BALANCE_ALERT" && sig == "" && ts == "" {
		bodySig := strings.TrimSpace(env.Signature)
		if bodySig == "" {
			bodySig = strings.TrimSpace(env.EventSignature)
		}
		bodyTS := strings.TrimSpace(env.Timestamp)
		if bodySig != "" && bodyTS != "" {
			for _, s := range candidateSecrets {
				if verifyPayoutLowBalanceBodySignature(s, raw, bodySig) {
					sig, ts = bodySig, bodyTS
					validSig = true
					usesBodySignature = true
					break
				}
			}
		}
	}

	if !validSig {
		slog.Warn("cashfree payout webhook rejected: invalid hmac signature",
			"remote_ip", c.ClientIP(),
			"timestamp", ts,
		)
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}

	tolerance := h.WebhookToleranceSec
	if tolerance <= 0 {
		tolerance = 300
	}
	if ts == "" || cashfree.VerifyWebhookTimestamp(ts, tolerance, time.Now().UTC()) != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing or expired webhook timestamp"})
		return
	}
	if usesBodySignature && evtTypePeek != "LOW_BALANCE_ALERT" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "body signatures are not valid for this event"})
		return
	}

	evtType := strings.ToUpper(strings.TrimSpace(env.Event))
	if evtType == "" {
		evtType = strings.ToUpper(strings.TrimSpace(env.Type))
	}
	if evtType == "" {
		evtType = "PAYOUT"
	}

	var data PayoutWebhookData
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &data)
	}

	// REQ-WH-001: persist the verified allow-listed payload before any payout mutation.
	audit := h.gatewayAuditStore()
	if audit == nil {
		slog.Error("payout webhook rejected: audit log unavailable")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "audit log service unavailable"})
		return
	}
	allowedAuditPayload := buildAllowedPayoutAuditPayload(env, data)
	evtRecord := &domain.WebhookEvent{
		Provider:         "cashfree",
		EventType:        evtType,
		Signature:        &sig,
		TimestampHeader:  &ts,
		RawPayload:       allowedAuditPayload,
		ProcessingStatus: "received",
	}
	if err := audit.CreateWebhookEvent(c.Request.Context(), evtRecord); err != nil {
		slog.Error("payout webhook forensic audit persistence failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "audit log persistence failed"})
		return
	}

	slog.Info("cashfree payout webhook received",
		"event", evtType,
		"transfer_id", data.TransferID,
		"batch_transfer_id", data.BatchTransferID,
		"status", data.Status,
		"status_code", data.StatusCode,
	)

	repo := h.getPayoutWebhookRepo()
	if repo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "payout repository unavailable"})
		return
	}

	ctx := c.Request.Context()

	// Handle BULK_TRANSFER_REJECTED
	if evtType == "BULK_TRANSFER_REJECTED" {
		rawBatchID := strings.TrimPrefix(data.BatchTransferID, "pgo_")
		if bID, err := uuid.Parse(rawBatchID); err == nil {
			slog.Error("CRITICAL: CASHFREE BULK TRANSFER REJECTED (setting batch failed)",
				"batch_id", bID,
				"reason", data.StatusDescription,
				"code", data.StatusCode,
			)
			if err := repo.SetBatchFailed(ctx, bID, fmt.Sprintf("bulk transfer rejected: %s", data.StatusDescription)); err != nil {
				slog.Error("failed to persist rejected payout batch status", "batch_id", bID, "err", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed recording rejected payout batch"})
				return
			}
			if h.OutboxEvents != nil {
				payload, marshalErr := json.Marshal(map[string]any{
					"batch_id": bID.String(),
					"reason":   data.StatusDescription,
					"code":     data.StatusCode,
				})
				if marshalErr != nil {
					slog.Error("failed to marshal rejected payout batch outbox", "batch_id", bID, "err", marshalErr)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed recording payout rejection event"})
					return
				}
				if err := h.OutboxEvents.InsertEvent(ctx, &domain.OutboxEvent{
					EventType: "payout_batch_rejected",
					Payload:   payload,
					CreatedAt: time.Now().UTC(),
				}); err != nil {
					slog.Error("failed to persist payout batch rejection outbox", "batch_id", bID, "err", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed recording payout rejection event"})
					return
				}
			}
		}
		c.Status(http.StatusOK)
		return
	}

	// Handle LOW_BALANCE_ALERT
	if evtType == "LOW_BALANCE_ALERT" {
		slog.Error("CRITICAL: CASHFREE PAYOUT LOW BALANCE ALERT RECEIVED",
			"event_type", evtType,
			"description", data.StatusDescription,
		)
		if h.OutboxEvents != nil {
			payload, marshalErr := json.Marshal(map[string]any{
				"event_type":   evtType,
				"description":  data.StatusDescription,
				"alert_status": data.Status,
				"received_at":  time.Now().UTC(),
			})
			if marshalErr != nil {
				slog.Error("failed to marshal payout low-balance alert", "err", marshalErr)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed recording payout low-balance alert"})
				return
			}
			if err := h.OutboxEvents.InsertEvent(ctx, &domain.OutboxEvent{
				EventType: "payout_low_balance_alert",
				Payload:   payload,
				CreatedAt: time.Now().UTC(),
			}); err != nil {
				slog.Error("failed to persist payout low-balance alert", "err", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed recording payout low-balance alert"})
				return
			}
		}
		c.Status(http.StatusOK)
		return
	}

	// Resolve target payout item
	rawTransferID := strings.TrimPrefix(data.TransferID, "pgo_")
	if idx := strings.IndexAny(rawTransferID, "_"); idx != -1 {
		rawTransferID = rawTransferID[:idx]
	}
	itemID, err := uuid.Parse(rawTransferID)
	if err != nil {
		// Not an item-level event or non-UUID reference; acknowledge safely
		c.Status(http.StatusOK)
		return
	}

	item, err := repo.GetPayoutItemByID(ctx, itemID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			slog.Warn("payout webhook: item not found", "item_id", itemID)
			c.Status(http.StatusOK)
			return
		}
		slog.Error("payout webhook: failed querying payout item", "item_id", itemID, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed resolving payout item"})
		return
	}
	if item == nil {
		slog.Warn("payout webhook: item not found", "item_id", itemID)
		c.Status(http.StatusOK)
		return
	}

	cfID := data.CFTransferID
	var propID uuid.UUID
	if item.BatchID != nil {
		b, err := repo.GetBatchByID(ctx, *item.BatchID)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				slog.Error("payout webhook: failed to get batch", "batch_id", *item.BatchID, "err", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed resolving batch"})
				return
			}
		} else if b != nil {
			propID = b.PropertyID
		}
	}
	if propID == uuid.Nil && item.PayeeID != uuid.Nil {
		p, err := repo.GetPayeeByID(ctx, item.PayeeID)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				slog.Error("payout webhook: failed to get payee", "payee_id", item.PayeeID, "err", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed resolving payee"})
				return
			}
		} else if p != nil {
			propID = p.PropertyID
		}
	}

	// Stable event timestamp across retries
	now := time.Now().UTC()
	occurredAt := now
	if data.SettledAt != nil && *data.SettledAt != "" {
		if t, err := time.Parse(time.RFC3339, *data.SettledAt); err == nil {
			occurredAt = t
		}
	} else if env.EventTime != "" {
		if t, err := time.Parse(time.RFC3339, env.EventTime); err == nil {
			occurredAt = t
		}
	}

	switch evtType {
	case "TRANSFER_SUCCESS", "TRANSFER_ACKNOWLEDGED":
		// Transfers V2 may first report TRANSFER_SUCCESS with SENT_TO_BENEFICIARY.
		// That is not beneficiary receipt. TRANSFER_ACKNOWLEDGED is the terminal
		// bank-credit confirmation; both settlement paths still require SUCCESS,
		// COMPLETED, and an exact match to the server-approved amount.
		if (data.Acknowledged != nil && *data.Acknowledged == 0) || !strings.EqualFold(data.Status, "SUCCESS") || !strings.EqualFold(data.StatusCode, "COMPLETED") {
			slog.Warn("payout webhook success not confirmed: status or status_code not completed",
				"item_id", item.ID,
				"event", evtType,
				"status", data.Status,
				"status_code", data.StatusCode,
				"acknowledged", data.Acknowledged,
			)
			if evtRecord.ID != uuid.Nil {
				_ = audit.UpdateWebhookEventStatus(ctx, evtRecord.ID, "ignored_unconfirmed", nil)
			}
			c.Status(http.StatusOK)
			return
		}
		if item.Status == domain.PayoutFailed || item.Status == domain.PayoutRejected || item.Status == domain.PayoutReversed || item.Status == domain.PayoutRetriableFailed {
			// Do not silently reopen a terminal failure on a late callback. The
			// operator-facing reconciler must resolve this contradictory history.
			slog.Error("payout webhook success conflicts with terminal local status; reconciliation required",
				"item_id", item.ID, "event", evtType, "local_status", item.Status)
			if evtRecord.ID != uuid.Nil {
				_ = audit.UpdateWebhookEventStatus(ctx, evtRecord.ID, "terminal_status_conflict", nil)
			}
			c.Status(http.StatusOK)
			return
		}
		if strings.TrimSpace(data.TransferAmount.String()) == "" {
			slog.Error("payout webhook settlement missing transfer_amount", "item_id", item.ID, "event", evtType)
			if evtRecord.ID != uuid.Nil {
				_ = audit.UpdateWebhookEventStatus(ctx, evtRecord.ID, "missing_transfer_amount", nil)
			}
			c.Status(http.StatusOK)
			return
		}
		providerAmountPaise, amountErr := cashfree.ParseRupeesToPaise(data.TransferAmount.String())
		if amountErr != nil || providerAmountPaise != item.AmountPaise {
			slog.Error("payout webhook settlement amount mismatch; refusing ledger/status mutation",
				"item_id", item.ID, "expected_paise", item.AmountPaise,
				"provider_amount", data.TransferAmount.String(), "parse_error", amountErr)
			if evtRecord.ID != uuid.Nil {
				_ = audit.UpdateWebhookEventStatus(ctx, evtRecord.ID, "amount_mismatch", nil)
			}
			c.Status(http.StatusOK)
			return
		}

		// Idempotency: If already succeeded, return 200 without duplicate ledger mirror
		if item.Status == domain.PayoutSucceeded {
			if evtRecord.ID != uuid.Nil {
				_ = audit.UpdateWebhookEventStatus(ctx, evtRecord.ID, "duplicate", nil)
			}
			c.Status(http.StatusOK)
			return
		}

		// Double-entry ledger mirror: Debit refund_payable, Credit bank
		// Ledger write must succeed or be durable in outbox before status is updated to Succeeded.
		if h.Finance != nil {
			if propID == uuid.Nil {
				slog.Error("payout webhook: unresolved property for settlement ledger event", "item_id", item.ID)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "unresolved property for ledger event"})
				return
			}
			if err := h.Finance.MirrorPayoutSettled(ctx, propID, item.ID, item.AmountPaise, occurredAt); err != nil {
				slog.Error("failed mirroring settled payout journal, attempting outbox fallback", "item_id", item.ID, "err", err)
				payload, mErr := json.Marshal(domain.PayoutSettlementMirrorPayload{
					PropertyID:   propID,
					PayoutItemID: item.ID,
					AmountPaise:  item.AmountPaise,
					OccurredAt:   occurredAt,
				})
				if mErr != nil {
					slog.Error("failed marshaling outbox payload for settled payout", "item_id", item.ID, "err", mErr)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "marshal outbox payload failed"})
					return
				}
				if oErr := h.enqueueLedgerOutboxEvent(ctx, propID, domain.LedgerOutboxPayoutSettlement, item.ID, payload, fmt.Sprintf("payout_settled:%s", item.ID)); oErr != nil {
					slog.Error("outbox fallback failed for settled payout", "item_id", item.ID, "err", oErr)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "ledger outbox persistence failed"})
					return
				}
			}
		}

		utr := ""
		if data.TransferUTR != nil {
			utr = *data.TransferUTR
		} else if data.UTR != nil {
			utr = *data.UTR
		}

		if !h.commitPayoutItem(c, ctx, audit, evtRecord, repo, evtType, payoutEventRef(data, item.ID), item.ID, domain.PayoutSucceeded, &cfID, &utr, &occurredAt, nil, item.BatchID) {
			return
		}

	case "TRANSFER_FAILED":
		reason := data.StatusDescription
		if reason == "" {
			reason = data.StatusCode
		}
		targetStatus := domain.PayoutFailed
		if data.StatusCode == "WAIT_TIME_EXCEEDED" {
			targetStatus = domain.PayoutRetriableFailed
		}

		if !h.commitPayoutItem(c, ctx, audit, evtRecord, repo, evtType, payoutEventRef(data, item.ID), item.ID, targetStatus, &cfID, nil, nil, &reason, item.BatchID) {
			return
		}

	case "TRANSFER_REVERSED":
		// Idempotency: If already reversed, return 200 without duplicate ledger mirror
		if item.Status == domain.PayoutReversed {
			if evtRecord.ID != uuid.Nil {
				_ = audit.UpdateWebhookEventStatus(ctx, evtRecord.ID, "duplicate", nil)
			}
			c.Status(http.StatusOK)
			return
		}

		// Double-entry ledger mirror: Debit bank, Credit refund_payable
		// Ledger write must succeed or be durable in outbox before status is updated to Reversed.
		if h.Finance != nil {
			if propID == uuid.Nil {
				slog.Error("payout webhook: unresolved property for reversal ledger event", "item_id", item.ID)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "unresolved property for ledger event"})
				return
			}
			if err := h.Finance.MirrorPayoutReversed(ctx, propID, item.ID, item.AmountPaise, occurredAt); err != nil {
				slog.Error("failed mirroring reversed payout journal, attempting outbox fallback", "item_id", item.ID, "err", err)
				payload, mErr := json.Marshal(domain.PayoutReversalMirrorPayload{
					PropertyID:   propID,
					PayoutItemID: item.ID,
					AmountPaise:  item.AmountPaise,
					OccurredAt:   occurredAt,
				})
				if mErr != nil {
					slog.Error("failed marshaling outbox payload for reversed payout", "item_id", item.ID, "err", mErr)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "marshal outbox payload failed"})
					return
				}
				if oErr := h.enqueueLedgerOutboxEvent(ctx, propID, domain.LedgerOutboxPayoutReversal, item.ID, payload, fmt.Sprintf("payout_reversed:%s", item.ID)); oErr != nil {
					slog.Error("outbox fallback failed for reversed payout", "item_id", item.ID, "err", oErr)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "ledger outbox persistence failed"})
					return
				}
			}
		}

		reason := "transfer reversed by bank"
		if data.StatusDescription != "" {
			reason = data.StatusDescription
		}
		if !h.commitPayoutItem(c, ctx, audit, evtRecord, repo, evtType, payoutEventRef(data, item.ID), item.ID, domain.PayoutReversed, &cfID, nil, nil, &reason, item.BatchID) {
			return
		}

	case "TRANSFER_REJECTED":
		reason := data.StatusDescription
		if reason == "" {
			reason = "transfer rejected by gateway"
		}
		if !h.commitPayoutItem(c, ctx, audit, evtRecord, repo, evtType, payoutEventRef(data, item.ID), item.ID, domain.PayoutRejected, &cfID, nil, nil, &reason, item.BatchID) {
			return
		}

	}

	if evtRecord.ID != uuid.Nil {
		_ = audit.UpdateWebhookEventStatus(ctx, evtRecord.ID, "processed", nil)
	}
	c.Status(http.StatusOK)
}

var errPayoutEventDuplicate = errors.New("payout webhook duplicate")

func payoutEventRef(data PayoutWebhookData, itemID uuid.UUID) string {
	if id := strings.TrimSpace(data.CFTransferID); id != "" {
		return id
	}
	if id := strings.TrimSpace(data.TransferID); id != "" {
		return id
	}
	return itemID.String()
}

func (h *Handlers) gatewayAuditStore() GatewayPaymentRepo {
	if h.GatewayPaymentRepo != nil {
		return h.GatewayPaymentRepo
	}
	if h.Pool != nil {
		return postgres.NewPaymentRepo(h.Pool)
	}
	return nil
}

// commitPayoutItem records the processed event and the status change together.
// It returns false after writing the HTTP response.
func (h *Handlers) commitPayoutItem(
	c *gin.Context,
	ctx context.Context,
	audit GatewayPaymentRepo,
	evtRecord *domain.WebhookEvent,
	repo PayoutWebhookStore,
	evtType, refID string,
	itemID uuid.UUID,
	status domain.PayoutItemStatus,
	cfTransferID, utr *string,
	settledAt *time.Time,
	failureReason *string,
	batchID *uuid.UUID,
) bool {
	run := func(tx pgx.Tx) error {
		var first bool
		var err error
		if tx != nil {
			first, err = postgres.NewPaymentRepo(tx).RecordProcessedEvent(ctx, "cashfree", evtType, refID, "")
		} else if audit != nil {
			first, err = audit.RecordProcessedEvent(ctx, "cashfree", evtType, refID, "")
		} else {
			return fmt.Errorf("audit store unavailable")
		}
		if err != nil {
			return err
		}
		if !first {
			return errPayoutEventDuplicate
		}
		if err := repo.UpdatePayoutItemStatusTx(ctx, tx, itemID, status, cfTransferID, utr, settledAt, failureReason); err != nil {
			return err
		}
		if batchID != nil {
			if _, rollErr := repo.UpdateBatchStatusFromItemsTx(ctx, tx, *batchID); rollErr != nil {
				slog.Error("failed rolling up batch status after payout webhook", "batch_id", *batchID, "err", rollErr)
			}
		}
		return nil
	}

	var err error
	if h.Pool != nil {
		err = postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error { return run(tx) })
	} else {
		err = run(nil)
	}
	if errors.Is(err, errPayoutEventDuplicate) {
		if evtRecord != nil && evtRecord.ID != uuid.Nil && audit != nil {
			_ = audit.UpdateWebhookEventStatus(ctx, evtRecord.ID, "duplicate", nil)
		}
		c.Status(http.StatusOK)
		return false
	}
	if err != nil {
		slog.Error("failed updating payout item", "item_id", itemID, "status", status, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update item failed"})
		return false
	}
	return true
}

func (h *Handlers) getPayoutWebhookRepo() PayoutWebhookStore {
	if h.PayoutWebhookRepo != nil {
		return h.PayoutWebhookRepo
	}
	repo := h.getPayoutRepo()
	if repo == nil {
		return nil
	}
	return repo
}

func (h *Handlers) enqueueLedgerOutboxEvent(ctx context.Context, propID uuid.UUID, evtType string, sourceID uuid.UUID, payload []byte, idempotencyKey string) error {
	if h.LedgerOutboxRepo == nil || h.Pool == nil {
		return fmt.Errorf("ledger outbox repo or pool unavailable for fallback")
	}
	outboxEvt := &domain.LedgerOutboxEvent{
		EventType:      evtType,
		PropertyID:     propID,
		SourceID:       sourceID,
		Payload:        payload,
		IdempotencyKey: idempotencyKey,
		MaxAttempts:    5,
	}
	return postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
		return h.LedgerOutboxRepo.InsertLedgerOutboxEventTx(ctx, tx, outboxEvt)
	})
}
