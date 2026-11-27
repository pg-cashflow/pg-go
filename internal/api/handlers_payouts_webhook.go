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
	"github.com/pg-cashflow/pg-go/internal/finance"
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
	candidateSecrets := cashfree.OrderedCandidateSecrets(h.CashfreePayoutWebhookSecret, h.CashfreePayoutWebhookSecrets)
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
	var unmarshalErr error
	if len(env.Data) > 0 {
		unmarshalErr = json.Unmarshal(env.Data, &data)
	}

	// Idempotency suppression: If Cashfree retries an already-processed payout event
	// (e.g. at Attempt 3: +10m, Attempt 4: +30m), return 200 OK before the timestamp check
	// to prevent Cashfree from disabling the webhook endpoint after 5 consecutive 401s.
	auditStore := h.gatewayAuditStore()
	if auditStore != nil && unmarshalErr == nil {
		if refID := payoutEventRef(data, uuid.Nil); refID != uuid.Nil.String() {
			alreadyProcessed, err := auditStore.HasProcessedEvent(c.Request.Context(), "cashfree", evtType, refID, "")
			if err == nil && alreadyProcessed {
				slog.Info("cashfree payout webhook duplicate delivery suppressed by transfer_id", "transfer_id", refID, "event", evtType)
				c.Status(http.StatusOK)
				return
			}
		}
	}

	tolerance := h.WebhookToleranceSec
	if tolerance <= 0 {
		tolerance = 300
	}
	if ts == "" || cashfree.VerifyWebhookTimestamp(ts, tolerance, time.Now().UTC()) != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing or expired webhook timestamp"})
		return
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

	if unmarshalErr != nil {
		errMsg := unmarshalErr.Error()
		updateWebhookAuditStatus(c.Request.Context(), audit, evtRecord, "dead_letter", &errMsg)
		slog.Error("payout webhook dead-letter: malformed data payload", "err", unmarshalErr, "event_type", evtType)
		c.Status(http.StatusOK)
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
			slog.Error("CRITICAL: CASHFREE BULK TRANSFER REJECTED (recording observation without terminating polling)",
				"batch_id", bID,
				"reason", data.StatusDescription,
				"code", data.StatusCode,
			)
			reason := fmt.Sprintf("bulk transfer rejection observed: %s (code: %s)", data.StatusDescription, data.StatusCode)
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
			if atomicRecorder, ok := repo.(PayoutBulkRejectionRecorder); ok {
				if err := atomicRecorder.RecordBulkBatchRejectionAtomic(ctx, bID, "BULK_TRANSFER_REJECTED", reason, payload); err != nil {
					slog.Error("failed to atomically persist rejected payout batch incident and outbox", "batch_id", bID, "err", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed recording rejected payout batch observation"})
					return
				}
			} else {
				if err := repo.RecordBatchRejectionObservation(ctx, bID, reason); err != nil {
					slog.Error("failed to persist rejected payout batch observation", "batch_id", bID, "err", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed recording rejected payout batch observation"})
					return
				}
				if h.OutboxEvents != nil {
					if err := h.OutboxEvents.InsertEvent(ctx, &domain.OutboxEvent{
						EventType: string(domain.EvtPayoutBatchRejected),
						Payload:   payload,
						CreatedAt: time.Now().UTC(),
					}); err != nil {
						slog.Error("failed to persist payout batch rejection outbox", "batch_id", bID, "err", err)
						c.JSON(http.StatusInternalServerError, gin.H{"error": "failed recording payout rejection event"})
						return
					}
				}
			}
			// Trigger immediate item-level reconciliation if dispatcher is configured
			if h.PayoutDispatcher != nil {
				if pollErr := h.PayoutDispatcher.PollBatch(ctx, bID); pollErr != nil {
					slog.Warn("payout batch reconcile after bulk rejection deferred to background poller", "batch_id", bID, "err", pollErr)
				}
			}
		}
		updateWebhookAuditStatus(ctx, audit, evtRecord, "processed", nil)
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
		updateWebhookAuditStatus(ctx, audit, evtRecord, "processed", nil)
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
		// PAYOUT-WH-02: Missing or invalid transfer ID on item-level event must be moved to dead_letter.
		errMsg := fmt.Sprintf("invalid or missing transfer_id %q for event %s: %v", data.TransferID, evtType, err)
		slog.Error("payout webhook dead-letter: missing or invalid transfer_id", "transfer_id", data.TransferID, "event_type", evtType, "err", err)
		updateWebhookAuditStatus(ctx, audit, evtRecord, "dead_letter", &errMsg)
		c.Status(http.StatusOK)
		return
	}

	item, err := repo.GetPayoutItemByID(ctx, itemID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			errMsg := fmt.Sprintf("payout item %s not found for transfer_id %q event %s", itemID, data.TransferID, evtType)
			slog.Error("payout webhook dead-letter: item not found", "item_id", itemID, "transfer_id", data.TransferID, "event_type", evtType)
			updateWebhookAuditStatus(ctx, audit, evtRecord, "dead_letter", &errMsg)
			c.Status(http.StatusOK)
			return
		}
		slog.Error("payout webhook: failed querying payout item", "item_id", itemID, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed resolving payout item"})
		return
	}
	if item == nil {
		errMsg := fmt.Sprintf("payout item %s is nil for transfer_id %q event %s", itemID, data.TransferID, evtType)
		slog.Error("payout webhook dead-letter: item is nil", "item_id", itemID, "transfer_id", data.TransferID, "event_type", evtType)
		updateWebhookAuditStatus(ctx, audit, evtRecord, "dead_letter", &errMsg)
		c.Status(http.StatusOK)
		return
	}

	// Canonical transfer ID validation: incoming data.TransferID must equal DerivePayoutTransferID(*item) exactly.
	expectedTransferID := finance.DerivePayoutTransferID(*item)
	if data.TransferID != expectedTransferID {
		errMsg := fmt.Sprintf("transfer_id mismatch: incoming %q does not match canonical %q for item %s (event %s)", data.TransferID, expectedTransferID, item.ID, evtType)
		slog.Error("payout webhook: transfer_id mismatch; refusing status mutation",
			"incoming_transfer_id", data.TransferID,
			"expected_transfer_id", expectedTransferID,
			"item_id", item.ID,
			"event_type", evtType,
		)
		updateWebhookAuditStatus(ctx, audit, evtRecord, "dead_letter", &errMsg)
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
		// That is not beneficiary receipt. V2 settlement requires TRANSFER_ACKNOWLEDGED
		// with status=SUCCESS and status_code=COMPLETED. Legacy V1 acknowledged=1 is
		// accepted only when V2 status_code is absent on TRANSFER_SUCCESS.
		hasExplicitLegacyAck := evtType == "TRANSFER_SUCCESS" &&
			strings.TrimSpace(data.StatusCode) == "" &&
			data.Acknowledged != nil && *data.Acknowledged == 1
		isV2Completed := evtType == "TRANSFER_ACKNOWLEDGED" && strings.EqualFold(data.StatusCode, "COMPLETED")

		if !strings.EqualFold(data.Status, "SUCCESS") || (!isV2Completed && !hasExplicitLegacyAck) {
			slog.Warn("payout webhook success not confirmed: requires TRANSFER_ACKNOWLEDGED or acknowledged=1",
				"item_id", item.ID,
				"event", evtType,
				"status", data.Status,
				"status_code", data.StatusCode,
				"acknowledged", data.Acknowledged,
			)
			updateWebhookAuditStatus(ctx, audit, evtRecord, "ignored_unconfirmed", nil)
			c.Status(http.StatusOK)
			return
		}
		if item.Status == domain.PayoutFailed || item.Status == domain.PayoutRejected || item.Status == domain.PayoutReversed || item.Status == domain.PayoutRetriableFailed {
			// Do not silently reopen a terminal failure on a late callback. The
			// operator-facing reconciler must resolve this contradictory history.
			slog.Error("payout webhook success conflicts with terminal local status; reconciliation required",
				"item_id", item.ID, "event", evtType, "local_status", item.Status)
			updateWebhookAuditStatus(ctx, audit, evtRecord, "terminal_status_conflict", nil)
			c.Status(http.StatusOK)
			return
		}
		if strings.TrimSpace(data.TransferAmount.String()) == "" {
			slog.Error("payout webhook settlement missing transfer_amount", "item_id", item.ID, "event", evtType)
			updateWebhookAuditStatus(ctx, audit, evtRecord, "missing_transfer_amount", nil)
			c.Status(http.StatusOK)
			return
		}
		providerAmountPaise, amountErr := cashfree.ParseRupeesToPaise(data.TransferAmount.String())
		if amountErr != nil || providerAmountPaise != item.AmountPaise {
			slog.Error("payout webhook settlement amount mismatch; refusing ledger/status mutation",
				"item_id", item.ID, "expected_paise", item.AmountPaise,
				"provider_amount", data.TransferAmount.String(), "parse_error", amountErr)
			updateWebhookAuditStatus(ctx, audit, evtRecord, "amount_mismatch", nil)
			c.Status(http.StatusOK)
			return
		}

		// Idempotency: If already succeeded, return 200 without duplicate ledger mirror
		if item.Status == domain.PayoutSucceeded {
			updateWebhookAuditStatus(ctx, audit, evtRecord, "duplicate", nil)
			c.Status(http.StatusOK)
			return
		}

		if h.Finance != nil && propID == uuid.Nil {
			slog.Error("payout webhook: unresolved property for settlement ledger event", "item_id", item.ID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unresolved property for ledger event"})
			return
		}

		utr := ""
		if data.TransferUTR != nil {
			utr = *data.TransferUTR
		} else if data.UTR != nil {
			utr = *data.UTR
		}

		if !h.commitPayoutItem(c, ctx, audit, evtRecord, repo, evtType, payoutEventRef(data, item.ID), propID, item, domain.PayoutSucceeded, &cfID, &utr, &occurredAt, nil) {
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

		if !h.commitPayoutItem(c, ctx, audit, evtRecord, repo, evtType, payoutEventRef(data, item.ID), propID, item, targetStatus, &cfID, nil, nil, &reason) {
			return
		}

	case "TRANSFER_REVERSED":
		// Idempotency: If already reversed, return 200 without duplicate ledger mirror
		if item.Status == domain.PayoutReversed {
			updateWebhookAuditStatus(ctx, audit, evtRecord, "duplicate", nil)
			c.Status(http.StatusOK)
			return
		}

		if item.Status == domain.PayoutFailed || item.Status == domain.PayoutRejected || item.Status == domain.PayoutRetriableFailed {
			// Do not post reversal on an item that was already marked failed or rejected.
			// Contradictory event history requires operator reconciliation.
			slog.Error("payout webhook reversal conflicts with terminal local status; reconciliation required",
				"item_id", item.ID, "event", evtType, "local_status", item.Status)
			updateWebhookAuditStatus(ctx, audit, evtRecord, "terminal_status_conflict", nil)
			c.Status(http.StatusOK)
			return
		}

		if strings.TrimSpace(data.TransferAmount.String()) == "" {
			slog.Error("payout webhook reversal missing transfer_amount", "item_id", item.ID, "event", evtType)
			updateWebhookAuditStatus(ctx, audit, evtRecord, "missing_transfer_amount", nil)
			c.Status(http.StatusOK)
			return
		}
		providerAmountPaise, amountErr := cashfree.ParseRupeesToPaise(data.TransferAmount.String())
		if amountErr != nil || providerAmountPaise != item.AmountPaise {
			slog.Error("payout webhook reversal amount mismatch; refusing ledger/status mutation",
				"item_id", item.ID, "expected_paise", item.AmountPaise,
				"provider_amount", data.TransferAmount.String(), "parse_error", amountErr)
			updateWebhookAuditStatus(ctx, audit, evtRecord, "amount_mismatch", nil)
			c.Status(http.StatusOK)
			return
		}

		if h.Finance != nil && propID == uuid.Nil {
			slog.Error("payout webhook: unresolved property for reversal ledger event", "item_id", item.ID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "unresolved property for ledger event"})
			return
		}

		reason := "transfer reversed by bank"
		if data.StatusDescription != "" {
			reason = data.StatusDescription
		}
		if !h.commitPayoutItem(c, ctx, audit, evtRecord, repo, evtType, payoutEventRef(data, item.ID), propID, item, domain.PayoutReversed, &cfID, nil, &occurredAt, &reason) {
			return
		}

	case "TRANSFER_REJECTED":
		reason := data.StatusDescription
		if reason == "" {
			reason = "transfer rejected by gateway"
		}
		if !h.commitPayoutItem(c, ctx, audit, evtRecord, repo, evtType, payoutEventRef(data, item.ID), propID, item, domain.PayoutRejected, &cfID, nil, nil, &reason) {
			return
		}

	}

	updateWebhookAuditStatus(ctx, audit, evtRecord, "processed", nil)
	c.Status(http.StatusOK)
}

func updateWebhookAuditStatus(ctx context.Context, audit GatewayPaymentRepo, evtRecord *domain.WebhookEvent, status string, errMsg *string) {
	if evtRecord != nil && evtRecord.ID != uuid.Nil && audit != nil {
		if err := audit.UpdateWebhookEventStatus(ctx, evtRecord.ID, status, errMsg); err != nil {
			slog.Error("payout webhook: failed updating audit status", "event_id", evtRecord.ID, "status", status, "err", err)
		}
	}
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

// commitPayoutItem records the processed event, transitions the payout item,
// and enqueues any required ledger outbox events in one atomic transaction.
// It returns false after writing the HTTP response.
func (h *Handlers) commitPayoutItem(
	c *gin.Context,
	ctx context.Context,
	audit GatewayPaymentRepo,
	evtRecord *domain.WebhookEvent,
	repo PayoutWebhookStore,
	evtType, refID string,
	propID uuid.UUID,
	item *domain.PayoutItem,
	status domain.PayoutItemStatus,
	cfTransferID, utr *string,
	settledAt *time.Time,
	failureReason *string,
) bool {
	// Fail closed before RecordProcessedEvent or any status mutation when finance mirroring
	// is active for a settlement/reversal transition without a transaction-backed reconciler.
	if h.Finance != nil && (status == domain.PayoutSucceeded || status == domain.PayoutReversed) {
		decision := domain.EvaluatePayoutTransition(item.Status, status)
		if decision.EnqueueSettlement || decision.EnqueueReversal {
			if _, ok := repo.(PayoutOutboxReconciler); !ok || h.Pool == nil {
				slog.Error("payout webhook requires transaction-backed outbox reconciler when finance mirroring is active",
					"item_id", item.ID, "event", evtType, "target_status", status)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "transaction-backed payout reconciler unavailable"})
				return false
			}
		}
	}

	var conflict bool
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
		if tx != nil {
			if reconciler, ok := repo.(PayoutOutboxReconciler); ok {
				res, rErr := reconciler.ReconcilePayoutItemTx(ctx, tx, domain.PayoutReconcileParams{
					PropertyID:    propID,
					ItemID:        item.ID,
					AmountPaise:   item.AmountPaise,
					TargetStatus:  status,
					CFTransferID:  cfTransferID,
					UTR:           utr,
					SettledAt:     settledAt,
					FailureReason: failureReason,
					Source:        "webhook:" + evtType,
				})
				if rErr != nil {
					return rErr
				}
				if res != nil && res.Conflict {
					conflict = true
					slog.Error("payout webhook conflicts with terminal local status; reconciliation required",
						"item_id", item.ID, "event", evtType, "local_status", res.PriorStatus, "incoming_status", status)
				}
				if res != nil && res.Committed && h.Finance != nil && h.LedgerOutboxRepo == nil &&
					(status == domain.PayoutSucceeded || status == domain.PayoutReversed) {
					return fmt.Errorf("ledger outbox repo unavailable")
				}
				return nil
			}
		}

		// Fallback for mock stores without a PostgreSQL transaction.
		decision := domain.EvaluatePayoutTransition(item.Status, status)
		if decision.Conflict {
			conflict = true
			slog.Error("payout webhook conflicts with terminal local status; reconciliation required",
				"item_id", item.ID, "event", evtType, "local_status", item.Status, "incoming_status", status)
			return nil
		}
		if decision.NoOp {
			return nil
		}
		if err := repo.UpdatePayoutItemStatusTx(ctx, tx, item.ID, status, cfTransferID, utr, settledAt, failureReason); err != nil {
			return err
		}
		if item.BatchID != nil {
			if _, rollErr := repo.UpdateBatchStatusFromItemsTx(ctx, tx, *item.BatchID); rollErr != nil {
				slog.Error("failed rolling up batch status after payout webhook", "batch_id", *item.BatchID, "err", rollErr)
				return fmt.Errorf("roll up batch status %s: %w", *item.BatchID, rollErr)
			}
		}
		if h.Finance != nil && propID != uuid.Nil && item.AmountPaise > 0 {
			occurredAt := time.Now().UTC()
			if settledAt != nil && !settledAt.IsZero() {
				occurredAt = *settledAt
			}
			if decision.EnqueueSettlement {
				if mErr := h.Finance.MirrorPayoutSettled(ctx, propID, item.ID, item.AmountPaise, occurredAt); mErr != nil {
					return fmt.Errorf("mirror payout settled: %w", mErr)
				}
			}
			if decision.EnqueueReversal {
				if mErr := h.Finance.MirrorPayoutReversed(ctx, propID, item.ID, item.AmountPaise, occurredAt); mErr != nil {
					return fmt.Errorf("mirror payout reversed: %w", mErr)
				}
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
		updateWebhookAuditStatus(ctx, audit, evtRecord, "duplicate", nil)
		c.Status(http.StatusOK)
		return false
	}
	if err != nil {
		slog.Error("failed updating payout item", "item_id", item.ID, "status", status, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "update item failed"})
		return false
	}
	if conflict {
		updateWebhookAuditStatus(ctx, audit, evtRecord, "terminal_status_conflict", nil)
		c.Status(http.StatusOK)
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
