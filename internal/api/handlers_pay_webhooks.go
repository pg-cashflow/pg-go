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
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

const maxWebhookBody = 1 << 20

// CashfreeWebhook handles POST /webhooks/cashfree.
func (h *Handlers) CashfreeWebhook(c *gin.Context) {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxWebhookBody+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body"})
		return
	}
	if len(raw) > maxWebhookBody {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "body too large"})
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))

	// Fail closed if CashfreeSecret is unconfigured
	if h.CashfreeSecret == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "payment gateway not configured"})
		return
	}

	sig := c.GetHeader("x-webhook-signature")
	ts := c.GetHeader("x-webhook-timestamp")
	if !cashfree.VerifyWebhookHMAC(h.CashfreeSecret, ts, string(raw), sig) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}

	// Verify timestamp tolerance (drift check) with fail-closed default (300s)
	tolerance := h.WebhookToleranceSec
	if tolerance <= 0 {
		tolerance = 300
	}
	if err := cashfree.VerifyWebhookTimestamp(ts, tolerance, time.Now().UTC()); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "timestamp tolerance exceeded"})
		return
	}

	// Ingest raw arrival into webhook_events audit log
	evtRecord := &domain.WebhookEvent{
		Provider:         "cashfree",
		Signature:        &sig,
		TimestampHeader:  &ts,
		RawPayload:       raw,
		ProcessingStatus: "received",
	}
	if h.GatewayPaymentRepo == nil {
		slog.Default().Error("webhook processing rejected: GatewayPaymentRepo is nil (audit log unavailable)")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "audit log service unavailable"})
		return
	}
	if err := h.GatewayPaymentRepo.CreateWebhookEvent(c.Request.Context(), evtRecord); err != nil {
		slog.Default().Error("webhook forensic audit persistence failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "audit log persistence failed"})
		return
	}

	val, evtType, err := cashfree.ParseWebhook(raw)
	if err != nil {
		// DEAD-LETTER ISOLATION: mark error, alert owner, return 200 OK to stop gateway retry storms
		errMsg := err.Error()
		if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(c.Request.Context(), evtRecord.ID, "dead_letter", &errMsg)
		}
		slog.Default().Error("cashfree webhook dead-letter (unparseable payload)", "err", err)
		c.Status(http.StatusOK)
		return
	}

	evtRecord.EventType = evtType

	switch evtType {
	case "PAYMENT_SUCCESS_WEBHOOK":
		succ, ok := val.(cashfree.SuccessWebhook)
		if !ok {
			c.Status(http.StatusOK)
			return
		}
		h.handlePaymentSuccessWebhook(c, succ, evtRecord)

	case "PAYMENT_FAILED_WEBHOOK":
		fail, ok := val.(cashfree.FailedWebhook)
		if !ok {
			c.Status(http.StatusOK)
			return
		}
		h.handlePaymentFailedWebhook(c, fail, evtRecord)

	case "REFUND_STATUS_WEBHOOK", "AUTO_REFUND_STATUS_WEBHOOK":
		ref, ok := val.(cashfree.RefundWebhook)
		if !ok {
			c.Status(http.StatusOK)
			return
		}
		h.handleRefundWebhook(c, ref, evtRecord)

	case "DISPUTE_CREATED", "DISPUTE_CREATED_WEBHOOK", "PAYMENT_DISPUTE_CREATED", "PAYMENT_DISPUTE_CREATED_WEBHOOK", "DISPUTE_STATUS_UPDATE", "DISPUTE_STATUS_UPDATE_WEBHOOK":
		disp, ok := val.(cashfree.DisputeWebhook)
		if !ok {
			c.Status(http.StatusOK)
			return
		}
		h.handleDisputeWebhook(c, disp, evtRecord)

	default:
		if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(c.Request.Context(), evtRecord.ID, "ignored", nil)
		}
		c.Status(http.StatusOK)
	}
}

func (h *Handlers) handlePaymentSuccessWebhook(c *gin.Context, succ cashfree.SuccessWebhook, evtRecord *domain.WebhookEvent) {
	ctx := c.Request.Context()
	if h.IntentStore == nil {
		c.Status(http.StatusOK)
		return
	}

	// Verify payment status is SUCCESS (fail closed on empty or non-SUCCESS)
	status := strings.ToUpper(strings.TrimSpace(succ.PaymentStatus))
	if status != "SUCCESS" {
		reason := fmt.Sprintf("invalid payment_status: %q", succ.PaymentStatus)
		slog.Default().Error("ALERT: payment success webhook rejected (anomaly detected)", "order_id", succ.OrderID, "status", succ.PaymentStatus, "reason", reason)
		if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "rejected", &reason)
		}
		c.Status(http.StatusOK)
		return
	}

	// Verify currency is INR (fail closed on empty or non-INR)
	curr := strings.ToUpper(strings.TrimSpace(succ.PaymentCurrency))
	if curr != "INR" {
		reason := fmt.Sprintf("invalid payment_currency: %q", succ.PaymentCurrency)
		slog.Default().Error("ALERT: payment success webhook rejected (anomaly detected)", "order_id", succ.OrderID, "currency", succ.PaymentCurrency, "reason", reason)
		if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "rejected", &reason)
		}
		c.Status(http.StatusOK)
		return
	}

	intent, err := h.IntentStore.GetByOrderID(ctx, succ.OrderID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if h.GatewayPaymentRepo != nil {
				if err := h.GatewayPaymentRepo.RecordUnmatchedReceipt(ctx, succ.OrderID, succ.CFPaymentID, nil, int64(succ.AmountPaise), "unknown_order", evtRecord.RawPayload); err != nil {
					slog.Error("failed recording unmatched receipt", "order_id", succ.OrderID, "cf_payment_id", succ.CFPaymentID, "err", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "record unmatched receipt failed"})
					return
				}
			}
			if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
				_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "unmatched", nil)
			}
			c.Status(http.StatusOK)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	if succ.AmountPaise != intent.AmountPaise {
		if h.GatewayPaymentRepo != nil {
			if err := h.GatewayPaymentRepo.RecordUnmatchedReceipt(ctx, succ.OrderID, succ.CFPaymentID, &intent.ID, int64(succ.AmountPaise), "amount_mismatch", evtRecord.RawPayload); err != nil {
				slog.Error("failed recording unmatched receipt for amount mismatch", "order_id", succ.OrderID, "err", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "record unmatched receipt failed"})
				return
			}
			if evtRecord.ID != uuid.Nil {
				reason := "amount_mismatch"
				_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "unmatched", &reason)
			}
		}
		c.Status(http.StatusOK)
		return
	}

	txnID := succ.TxnID()
	if err := h.SettleGatewayOrder(ctx, succ.OrderID, succ.CFPaymentID, txnID, succ.AmountPaise, intent, evtRecord.RawPayload); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settle failed"})
		return
	}

	if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
		_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "processed", nil)
	}
	c.Status(http.StatusOK)
}

// SettleGatewayOrder executes canonical, idempotent settlement for a gateway payment.
// It is shared by both CashfreeWebhook and TenantDuePayVerify to guarantee identical locking,
// deduplication, multi-due allocation, and ledger outbox mirroring in both execution orders.
func (h *Handlers) SettleGatewayOrder(
	ctx context.Context,
	orderID string,
	cfPaymentID string,
	txnID string,
	amountPaise int64,
	intent *domain.PaymentIntent,
	rawPayload []byte,
) error {
	if h.OrderSettler != nil {
		return h.OrderSettler.SettleGatewayOrder(ctx, orderID, cfPaymentID, txnID, amountPaise, intent, rawPayload)
	}
	settler := h.getOrderSettler()
	if settler == nil {
		return errors.New("order settlement service unavailable")
	}
	return settler.SettleGatewayOrder(ctx, orderID, cfPaymentID, txnID, amountPaise, intent, rawPayload)
}

func (h *Handlers) getOrderSettler() payment.OrderGatewaySettler {
	if h.OrderSettler != nil {
		return h.OrderSettler
	}
	return payment.NewOrderSettlementService(payment.OrderSettlementConfig{
		Pool:               h.Pool,
		GatewayPaymentRepo: h.GatewayPaymentRepo,
		IntentStore:        h.IntentStore,
		LedgerOutboxRepo:   h.LedgerOutboxRepo,
		FinanceEnabled:     h.FinanceEnabled || h.Finance != nil,
		Payments:           h.Payments,
	})
}

func (h *Handlers) handlePaymentFailedWebhook(c *gin.Context, fail cashfree.FailedWebhook, evtRecord *domain.WebhookEvent) {
	ctx := c.Request.Context()
	if h.Pool != nil && h.GatewayPaymentRepo != nil {
		if txErr := postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
			txPayRepo := postgres.NewPaymentRepo(tx)
			firstSeen, err := txPayRepo.RecordProcessedEvent(ctx, "cashfree", "PAYMENT_FAILED_WEBHOOK", fail.CFPaymentID, "")
			if err != nil || !firstSeen {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE payment_intents SET status='failed', updated_at=NOW() WHERE provider_order_id=$1 AND status NOT IN ('paid', 'refunded')`, fail.OrderID); err != nil {
				return err
			}
			return nil
		}); txErr != nil {
			// Transaction failure on payment-failed webhook — the intent status update or dedup insert failed.
			// Log at ERROR for operator visibility; still return 200 OK to prevent gateway retry storms on
			// a non-money-moving event. The webhook_events audit record is the authoritative receipt.
			slog.Default().Error("PAYMENT_FAILED_WEBHOOK: transaction error updating payment_intents",
				"order_id", fail.OrderID,
				"cf_payment_id", fail.CFPaymentID,
				"err", txErr,
			)
		}
	}
	if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
		_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "processed", nil)
	}
	c.Status(http.StatusOK)
}

func (h *Handlers) handleRefundWebhook(c *gin.Context, ref cashfree.RefundWebhook, evtRecord *domain.WebhookEvent) {
	ctx := c.Request.Context()
	if h.GatewayPaymentRepo == nil {
		c.Status(http.StatusOK)
		return
	}

	// Step 0: Validate mandatory identity fields
	if (ref.CFRefundID == "" && ref.RefundID == "") || ref.OrderID == "" || ref.RefundAmount <= 0 {
		slog.Error("refund webhook: missing required identity fields",
			"order_id", ref.OrderID, "cf_refund_id", ref.CFRefundID, "refund_id", ref.RefundID, "amount", ref.RefundAmount)
		if err := h.GatewayPaymentRepo.RecordUnmatchedReceipt(ctx, ref.OrderID, ref.CFPaymentID, nil, ref.RefundAmount, "missing_required_identity_fields", evtRecord.RawPayload); err != nil {
			slog.Error("failed recording unmatched receipt for invalid refund webhook", "err", err)
		}
		if evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "unmatched", nil)
		}
		c.Status(http.StatusOK)
		return
	}

	// Step 1: Unlocked ancestry resolution
	var p *domain.Payment
	if ref.CFPaymentID != "" {
		if payment, err := h.GatewayPaymentRepo.GetByCFPaymentID(ctx, ref.CFPaymentID); err == nil && payment != nil {
			p = payment
		}
	}
	if p == nil && ref.OrderID != "" && h.IntentStore != nil {
		if intent, err := h.IntentStore.GetByOrderID(ctx, ref.OrderID); err == nil && intent != nil {
			if intent.CFPaymentID != nil && *intent.CFPaymentID != "" {
				if payment, err := h.GatewayPaymentRepo.GetByCFPaymentID(ctx, *intent.CFPaymentID); err == nil && payment != nil {
					p = payment
				}
			}
		}
	}

	var existingRef *domain.GatewayRefund
	if ref.RefundID != "" {
		var err error
		existingRef, err = h.GatewayPaymentRepo.GetRefundByReference(ctx, ref.RefundID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("refund webhook: failed to query refund by reference", "err", err, "refund_id", ref.RefundID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "database error resolving refund"})
			return
		}
	}
	if existingRef == nil && ref.CFRefundID != "" {
		var err error
		existingRef, err = h.GatewayPaymentRepo.GetRefundByCFRefundID(ctx, ref.CFRefundID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("refund webhook: failed to query refund by cf_refund_id", "err", err, "cf_refund_id", ref.CFRefundID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "database error resolving refund"})
			return
		}
	}

	// Validate ancestry scoping: if payment was resolved, existingRef must belong to that payment
	if p != nil && existingRef != nil && existingRef.PaymentID != p.ID {
		slog.Error("refund webhook: refund reference matches another payment; quarantining cross-payment conflict",
			"refund_id", ref.RefundID, "existing_payment_id", existingRef.PaymentID, "target_payment_id", p.ID)
		if err := h.GatewayPaymentRepo.RecordUnmatchedReceipt(ctx, ref.OrderID, ref.CFPaymentID, &p.ID, ref.RefundAmount, "refund_payment_mismatch", evtRecord.RawPayload); err != nil {
			slog.Error("failed recording unmatched receipt for refund mismatch", "order_id", ref.OrderID, "err", err)
		}
		if evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "unmatched", nil)
		}
		c.Status(http.StatusOK)
		return
	}

	// Validate amount scoping: existing refund row amount must match incoming webhook amount
	if existingRef != nil && existingRef.AmountPaise != ref.RefundAmount {
		slog.Error("refund webhook: refund amount mismatch with existing record; quarantining",
			"refund_id", ref.RefundID, "existing_amount", existingRef.AmountPaise, "webhook_amount", ref.RefundAmount)
		targetPayID := existingRef.PaymentID
		if err := h.GatewayPaymentRepo.RecordUnmatchedReceipt(ctx, ref.OrderID, ref.CFPaymentID, &targetPayID, ref.RefundAmount, "refund_amount_mismatch", evtRecord.RawPayload); err != nil {
			slog.Error("failed recording unmatched receipt for refund amount mismatch", "order_id", ref.OrderID, "err", err)
		}
		if evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "unmatched", nil)
		}
		c.Status(http.StatusOK)
		return
	}

	if p == nil && existingRef != nil {
		var err error
		p, err = h.GatewayPaymentRepo.GetByID(ctx, existingRef.PaymentID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			slog.Error("refund webhook: failed to query payment by id", "err", err, "payment_id", existingRef.PaymentID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "database error resolving payment"})
			return
		}
	}

	if p == nil {
		if err := h.GatewayPaymentRepo.RecordUnmatchedReceipt(ctx, ref.OrderID, ref.CFPaymentID, nil, ref.RefundAmount, "payment_not_found", evtRecord.RawPayload); err != nil {
			slog.Error("failed recording unmatched receipt for refund", "order_id", ref.OrderID, "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "record unmatched receipt failed"})
			return
		}
		if evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "unmatched", nil)
		}
		c.Status(http.StatusOK)
		return
	}

	if h.DueStore != nil && p.DueID != uuid.Nil {
		_, err := h.DueStore.GetByID(ctx, p.DueID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "due lookup failed"})
			return
		}
	}

	// Step 2: Lock hierarchy and forward-only status transition
	if h.Pool != nil {
		var rfRow *domain.GatewayRefund
		dbRefundStatus := domain.NormalizeRefundStatus(ref.RefundStatus)

		err := postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
			txPayRepo := postgres.NewPaymentRepo(tx)

			// In-transaction deduplication
			dedupeEntityID := ref.CFRefundID
			if dedupeEntityID == "" {
				dedupeEntityID = ref.RefundID
			}
			firstSeen, err := txPayRepo.RecordProcessedEvent(ctx, "cashfree", ref.Type, dedupeEntityID, ref.RefundStatus)
			if err != nil {
				return err
			}
			if !firstSeen {
				return nil
			}

			source := "system"
			if ref.IsAutoRefund {
				source = "cashfree_auto"
			}
			rfRow = &domain.GatewayRefund{
				PaymentID:       p.ID,
				CFRefundID:      &ref.CFRefundID,
				RefundReference: &ref.RefundID,
				AmountPaise:     ref.RefundAmount,
				Status:          dbRefundStatus,
				Source:          source,
				Reason:          ref.RefundReason,
				RawPayload:      evtRecord.RawPayload,
			}
			if existingRef != nil {
				rfRow.ID = existingRef.ID
				rfRow.PropertyID = existingRef.PropertyID
				rfRow.IdempotencyKey = existingRef.IdempotencyKey
				rfRow.InitiatedBy = existingRef.InitiatedBy
				rfRow.CreatedAt = existingRef.CreatedAt
			} else {
				rfRow.ID = uuid.New()
				if p.PropertyID != nil {
					rfRow.PropertyID = *p.PropertyID
				}
				rfRow.CreatedAt = time.Now().UTC()
			}

			if err := txPayRepo.ReconcileRefundTransition(ctx, rfRow, dbRefundStatus); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "refund settle failed"})
			return
		}

		if h.Referrals != nil && dbRefundStatus == domain.RefundStatusSucceeded {
			payRepo := postgres.NewPaymentRepo(h.Pool)
			targetRefundID := uuid.Nil
			if rfRow != nil {
				targetRefundID = rfRow.ID
			} else if existingRef != nil {
				targetRefundID = existingRef.ID
			}
			if targetRefundID != uuid.Nil {
				if dueIDs, err := payRepo.GetRefundDueIDs(ctx, targetRefundID); err == nil {
					for _, dueID := range dueIDs {
						if dueID != uuid.Nil {
							if err := h.Referrals.ReverseFirstRentPayment(ctx, dueID, p.ID); err != nil {
								slog.Warn("referral reverse on refund webhook failed", "due_id", dueID, "payment_id", p.ID, "err", err)
							}
						}
					}
				}
			}
		}
	} else if h.GatewayPaymentRepo != nil {
		source := "system"
		if ref.IsAutoRefund {
			source = "cashfree_auto"
		}
		dbRefundStatus := domain.NormalizeRefundStatus(ref.RefundStatus)
		rfRow := &domain.GatewayRefund{
			PaymentID:       p.ID,
			CFRefundID:      &ref.CFRefundID,
			RefundReference: &ref.RefundID,
			AmountPaise:     ref.RefundAmount,
			Status:          dbRefundStatus,
			Source:          source,
			Reason:          ref.RefundReason,
			RawPayload:      evtRecord.RawPayload,
		}
		if err := h.GatewayPaymentRepo.CreateOrUpdateRefund(ctx, rfRow); err != nil {
			slog.Error("failed updating refund status from webhook", "err", err, "cf_refund_id", ref.CFRefundID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update refund failed"})
			return
		}
	}

	if evtRecord.ID != uuid.Nil {
		_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "processed", nil)
	}
	c.Status(http.StatusOK)
}

func (h *Handlers) handleDisputeWebhook(c *gin.Context, disp cashfree.DisputeWebhook, evtRecord *domain.WebhookEvent) {
	ctx := c.Request.Context()
	slog.Default().Error("CASHFREE PAYMENT DISPUTE RECEIVED: OPERATOR ACTION REQUIRED",
		"dispute_id", disp.DisputeID,
		"order_id", disp.OrderID,
		"cf_payment_id", disp.CFPaymentID,
		"dispute_type", disp.DisputeType,
		"dispute_status", disp.DisputeStatus,
		"dispute_amount_paise", disp.DisputeAmount,
		"respond_by", disp.RespondBy,
		"reason_code", disp.ReasonCode,
		"reason_description", disp.ReasonDescription,
	)

	statusMsg := fmt.Sprintf("dispute_id=%s type=%s status=%s respond_by=%s", disp.DisputeID, disp.DisputeType, disp.DisputeStatus, disp.RespondBy)
	if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
		_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "dispute_action_required", &statusMsg)
	}

	if h.IntentStore != nil && disp.OrderID != "" {
		intent, err := h.IntentStore.GetByOrderID(ctx, disp.OrderID)
		if err == nil && intent != nil {
			var propID uuid.UUID
			var tenantID *uuid.UUID
			if h.DueStore != nil {
				if d, err := h.DueStore.GetByID(ctx, intent.DueID); err == nil && d != nil {
					propID = d.PropertyID
					tenantID = &d.TenantID
				}
			}

			payload, _ := json.Marshal(map[string]any{
				"order_id":             disp.OrderID,
				"dispute_id":           disp.DisputeID,
				"dispute_type":         disp.DisputeType,
				"dispute_status":       disp.DisputeStatus,
				"dispute_amount_paise": disp.DisputeAmount,
				"respond_by":           disp.RespondBy,
				"reason_description":   disp.ReasonDescription,
			})
			if h.Events != nil {
				_ = h.Events.Publish(ctx, domain.Event{
					PropertyID: propID,
					TenantID:   tenantID,
					EventType:  domain.EvtPaymentDisputed,
					OccurredAt: time.Now().UTC(),
					Payload:    payload,
				})
			}
			if h.OutboxEvents != nil {
				_ = h.OutboxEvents.InsertEvent(ctx, &domain.OutboxEvent{
					EventType:  string(domain.EvtPaymentDisputed),
					PropertyID: propID,
					TenantID:   tenantID,
					ActorRole:  string(domain.RoleOwner),
					Payload:    payload,
				})
			}
		}
	}

	// Fail-Safe Ledger Invariant: A dispute is a contested claim, NOT an authorized or confirmed refund.
	// We strictly refrain from mutating double-entry journals or resetting dues to pending automatically.
	// Returning HTTP 200 OK acknowledges the webhook delivery to prevent gateway retry storms.
	c.Status(http.StatusOK)
}
