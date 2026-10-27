package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
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
	Event     string          `json:"event"`
	Type      string          `json:"type"`
	EventTime string          `json:"event_time"`
	Data      json.RawMessage `json:"data"`
}

// PayoutWebhookData holds fields for individual or batch payout notifications.
type PayoutWebhookData struct {
	TransferID        string  `json:"transfer_id"`
	CFTransferID      string  `json:"cf_transfer_id"`
	BatchTransferID   string  `json:"batch_transfer_id"`
	CFBatchTransferID string  `json:"cf_batch_transfer_id"`
	Status            string  `json:"status"`
	StatusCode        string  `json:"status_code"`
	StatusDescription string  `json:"status_description"`
	UTR               *string `json:"utr"`
	AcknowledgedAt    *string `json:"acknowledged_at"`
	SettledAt         *string `json:"settled_at"`
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

	// Fail closed if CashfreePayoutWebhookSecret is unconfigured
	secret := h.CashfreePayoutWebhookSecret
	if secret == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "payout webhook secret not configured"})
		return
	}

	sig := c.GetHeader("x-webhook-signature")
	ts := c.GetHeader("x-webhook-timestamp")
	if !cashfree.VerifyWebhookHMAC(secret, ts, string(raw), sig) {
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
	if err := cashfree.VerifyWebhookTimestamp(ts, tolerance, time.Now().UTC()); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "timestamp tolerance exceeded"})
		return
	}

	// REQ-WH-001: persist the verified payload before any payout mutation.
	audit := h.gatewayAuditStore()
	if audit == nil {
		slog.Error("payout webhook rejected: audit log unavailable")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "audit log service unavailable"})
		return
	}
	evtRecord := &domain.WebhookEvent{
		Provider:         "cashfree",
		EventType:        "PAYOUT",
		Signature:        &sig,
		TimestampHeader:  &ts,
		RawPayload:       raw,
		ProcessingStatus: "received",
	}
	if err := audit.CreateWebhookEvent(c.Request.Context(), evtRecord); err != nil {
		slog.Error("payout webhook forensic audit persistence failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "audit log persistence failed"})
		return
	}

	var env PayoutWebhookEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		errMsg := err.Error()
		_ = audit.UpdateWebhookEventStatus(c.Request.Context(), evtRecord.ID, "dead_letter", &errMsg)
		slog.Error("cashfree payout webhook dead-letter: unparseable envelope json", "err", err)
		c.Status(http.StatusOK)
		return
	}

	evtType := strings.ToUpper(strings.TrimSpace(env.Event))
	if evtType == "" {
		evtType = strings.ToUpper(strings.TrimSpace(env.Type))
	}
	if evtType != "" {
		evtRecord.EventType = evtType
	}

	var data PayoutWebhookData
	if len(env.Data) > 0 {
		_ = json.Unmarshal(env.Data, &data)
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
			slog.Error("CRITICAL: CASHFREE BULK TRANSFER REJECTED",
				"batch_id", bID,
				"reason", data.StatusDescription,
				"code", data.StatusCode,
			)
			_ = repo.SetBatchDispatchUnknown(ctx, bID, fmt.Sprintf("bulk transfer rejected: %s", data.StatusDescription))
		}
		c.Status(http.StatusOK)
		return
	}

	// Resolve target payout item
	rawTransferID := strings.TrimPrefix(data.TransferID, "pgo_")
	parts := strings.Split(rawTransferID, "_r")
	itemID, err := uuid.Parse(parts[0])
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
	case "TRANSFER_SUCCESS":
		// Cashfree Transfers V2: A transfer is settled only when status is SUCCESS and status_code is COMPLETED.
		if !strings.EqualFold(data.Status, "SUCCESS") || !strings.EqualFold(data.StatusCode, "COMPLETED") {
			slog.Warn("payout webhook TRANSFER_SUCCESS unconfirmed: status or status_code not completed",
				"item_id", item.ID,
				"status", data.Status,
				"status_code", data.StatusCode,
			)
			if evtRecord.ID != uuid.Nil {
				_ = audit.UpdateWebhookEventStatus(ctx, evtRecord.ID, "ignored_unconfirmed", nil)
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
		if data.UTR != nil {
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

	case "TRANSFER_ACKNOWLEDGED":
		// Gateway accepted transfer into in-flight queue; keep processing
		if !h.commitPayoutItem(c, ctx, audit, evtRecord, repo, evtType, payoutEventRef(data, item.ID), item.ID, domain.PayoutProcessing, &cfID, nil, nil, nil, item.BatchID) {
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
