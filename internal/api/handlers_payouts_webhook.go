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

	var env PayoutWebhookEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		slog.Error("cashfree payout webhook dead-letter: unparseable envelope json", "err", err)
		c.Status(http.StatusOK)
		return
	}

	evtType := strings.ToUpper(strings.TrimSpace(env.Event))
	if evtType == "" {
		evtType = strings.ToUpper(strings.TrimSpace(env.Type))
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
		// Idempotency: If already succeeded, return 200 without duplicate ledger mirror
		if item.Status == domain.PayoutSucceeded {
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

		if err := repo.UpdatePayoutItemStatusTx(ctx, nil, item.ID, domain.PayoutSucceeded, &cfID, &utr, &occurredAt, nil); err != nil {
			slog.Error("failed updating payout item to succeeded", "item_id", item.ID, "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update item failed"})
			return
		}

		if item.BatchID != nil {
			if _, err := repo.UpdateBatchStatusFromItemsTx(ctx, nil, *item.BatchID); err != nil {
				slog.Error("failed rolling up batch status after item succeeded", "batch_id", *item.BatchID, "err", err)
			}
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

		if err := repo.UpdatePayoutItemStatusTx(ctx, nil, item.ID, targetStatus, &cfID, nil, nil, &reason); err != nil {
			slog.Error("failed updating payout item to failed", "item_id", item.ID, "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update item failed"})
			return
		}
		if item.BatchID != nil {
			// Rollup is advisory: a batch-status failure does not block gateway retry of the item event.
			if _, err := repo.UpdateBatchStatusFromItemsTx(ctx, nil, *item.BatchID); err != nil {
				slog.Error("failed rolling up batch status after item failed", "batch_id", *item.BatchID, "err", err)
			}
		}

	case "TRANSFER_REVERSED":
		// Idempotency: If already reversed, return 200 without duplicate ledger mirror
		if item.Status == domain.PayoutReversed {
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
		if err := repo.UpdatePayoutItemStatusTx(ctx, nil, item.ID, domain.PayoutReversed, &cfID, nil, nil, &reason); err != nil {
			slog.Error("failed updating payout item to reversed", "item_id", item.ID, "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update item failed"})
			return
		}
		if item.BatchID != nil {
			// Rollup is advisory: a batch-status failure does not block gateway retry of the item event.
			if _, err := repo.UpdateBatchStatusFromItemsTx(ctx, nil, *item.BatchID); err != nil {
				slog.Error("failed rolling up batch status after item reversed", "batch_id", *item.BatchID, "err", err)
			}
		}

	case "TRANSFER_REJECTED":
		reason := data.StatusDescription
		if reason == "" {
			reason = "transfer rejected by gateway"
		}
		if err := repo.UpdatePayoutItemStatusTx(ctx, nil, item.ID, domain.PayoutRejected, &cfID, nil, nil, &reason); err != nil {
			slog.Error("failed updating payout item to rejected", "item_id", item.ID, "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update item failed"})
			return
		}
		if item.BatchID != nil {
			// Rollup is advisory: a batch-status failure does not block gateway retry of the item event.
			if _, err := repo.UpdateBatchStatusFromItemsTx(ctx, nil, *item.BatchID); err != nil {
				slog.Error("failed rolling up batch status after item rejected", "batch_id", *item.BatchID, "err", err)
			}
		}

	case "TRANSFER_ACKNOWLEDGED":
		// Gateway accepted transfer into in-flight queue; keep processing
		if err := repo.UpdatePayoutItemStatusTx(ctx, nil, item.ID, domain.PayoutProcessing, &cfID, nil, nil, nil); err != nil {
			slog.Error("failed updating payout item to processing", "item_id", item.ID, "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update item failed"})
			return
		}
	}

	c.Status(http.StatusOK)
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
