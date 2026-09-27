package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
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

	// Defense-in-depth: Validate HMAC signature against dedicated CF_PAYOUT_WEBHOOK_SECRET
	secret := h.CashfreePayoutWebhookSecret
	if secret == "" {
		if h.PayoutRepo != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "payout webhook secret not configured"})
			return
		}
		c.Status(http.StatusOK)
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

	if h.WebhookToleranceSec > 0 {
		if err := cashfree.VerifyWebhookTimestamp(ts, h.WebhookToleranceSec, time.Now().UTC()); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "timestamp tolerance exceeded"})
			return
		}
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

	repo := h.getPayoutRepo()
	if repo == nil {
		c.Status(http.StatusOK)
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
	if err != nil || item == nil {
		slog.Warn("payout webhook: item not found", "item_id", itemID)
		c.Status(http.StatusOK)
		return
	}

	cfID := data.CFTransferID
	var propID uuid.UUID
	if item.BatchID != nil {
		if b, err := repo.GetBatchByID(ctx, *item.BatchID); err == nil && b != nil {
			propID = b.PropertyID
		}
	}

	switch evtType {
	case "TRANSFER_SUCCESS":
		// Idempotency: If already succeeded, return 200 without duplicate ledger mirror
		if item.Status == domain.PayoutSucceeded {
			c.Status(http.StatusOK)
			return
		}

		utr := ""
		if data.UTR != nil {
			utr = *data.UTR
		}
		now := time.Now().UTC()
		settledAt := now
		if data.SettledAt != nil && *data.SettledAt != "" {
			if t, err := time.Parse(time.RFC3339, *data.SettledAt); err == nil {
				settledAt = t
			}
		}

		if err := repo.UpdatePayoutItemStatusTx(ctx, nil, item.ID, domain.PayoutSucceeded, &cfID, &utr, &settledAt, nil); err != nil {
			slog.Error("failed updating payout item to succeeded", "item_id", item.ID, "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update item failed"})
			return
		}

		if item.BatchID != nil {
			_, _ = repo.UpdateBatchStatusFromItemsTx(ctx, nil, *item.BatchID)
		}

		// Double-entry ledger mirror: Debit refund_payable, Credit bank
		if h.Finance != nil && propID != uuid.Nil {
			if err := h.Finance.MirrorPayoutSettled(ctx, propID, item.ID, item.AmountPaise, settledAt); err != nil {
				slog.Error("failed mirroring settled payout journal", "item_id", item.ID, "err", err)
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

		_ = repo.UpdatePayoutItemStatusTx(ctx, nil, item.ID, targetStatus, &cfID, nil, nil, &reason)
		if item.BatchID != nil {
			_, _ = repo.UpdateBatchStatusFromItemsTx(ctx, nil, *item.BatchID)
		}

	case "TRANSFER_REVERSED":
		// Money returned: Distinct accounting reversal entry (Debit bank, Credit refund_payable)
		reason := "transfer reversed by bank"
		if data.StatusDescription != "" {
			reason = data.StatusDescription
		}
		_ = repo.UpdatePayoutItemStatusTx(ctx, nil, item.ID, domain.PayoutReversed, &cfID, nil, nil, &reason)
		if item.BatchID != nil {
			_, _ = repo.UpdateBatchStatusFromItemsTx(ctx, nil, *item.BatchID)
		}

		if h.Finance != nil && propID != uuid.Nil {
			if err := h.Finance.MirrorPayoutReversed(ctx, propID, item.ID, item.AmountPaise, time.Now().UTC()); err != nil {
				slog.Error("failed mirroring reversed payout journal", "item_id", item.ID, "err", err)
			}
		}

	case "TRANSFER_REJECTED":
		reason := data.StatusDescription
		if reason == "" {
			reason = "transfer rejected by gateway"
		}
		_ = repo.UpdatePayoutItemStatusTx(ctx, nil, item.ID, domain.PayoutRejected, &cfID, nil, nil, &reason)
		if item.BatchID != nil {
			_, _ = repo.UpdateBatchStatusFromItemsTx(ctx, nil, *item.BatchID)
		}

	case "TRANSFER_ACKNOWLEDGED":
		// Gateway accepted transfer into in-flight queue; keep processing
		_ = repo.UpdatePayoutItemStatusTx(ctx, nil, item.ID, domain.PayoutProcessing, &cfID, nil, nil, nil)
	}

	c.Status(http.StatusOK)
}
