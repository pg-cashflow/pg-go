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

// buildAllowedPaymentAuditPayload extracts allow-listed operational fields from payment webhooks
// for audit persistence, stripping payer phone/email, card numbers, and authorization headers.
func buildAllowedPaymentAuditPayload(evtType string, val any) []byte {
	m := map[string]any{
		"event_type": evtType,
	}
	switch v := val.(type) {
	case cashfree.SuccessWebhook:
		m["data"] = map[string]any{
			"type":             v.Type,
			"order_id":         v.OrderID,
			"cf_payment_id":    v.CFPaymentID,
			"bank_reference":   v.BankReference,
			"amount_paise":     v.AmountPaise,
			"payment_status":   v.PaymentStatus,
			"payment_currency": v.PaymentCurrency,
			"payment_time":     v.PaymentTime,
		}
	case cashfree.FailedWebhook:
		m["data"] = map[string]any{
			"type":           v.Type,
			"order_id":       v.OrderID,
			"cf_payment_id":  v.CFPaymentID,
			"amount_paise":   v.AmountPaise,
			"payment_status": v.PaymentStatus,
			"failure_reason": v.FailureReason,
		}
	case cashfree.RefundWebhook:
		m["data"] = map[string]any{
			"type":           v.Type,
			"is_auto_refund": v.IsAutoRefund,
			"cf_refund_id":   v.CFRefundID,
			"refund_id":      v.RefundID,
			"order_id":       v.OrderID,
			"cf_payment_id":  v.CFPaymentID,
			"refund_status":  v.RefundStatus,
			"refund_amount":  v.RefundAmount,
			"refund_type":    v.RefundType,
			"refund_reason":  v.RefundReason,
		}
	case cashfree.DisputeWebhook:
		m["data"] = map[string]any{
			"type":               v.Type,
			"dispute_id":         v.DisputeID,
			"order_id":           v.OrderID,
			"cf_payment_id":      v.CFPaymentID,
			"dispute_type":       v.DisputeType,
			"dispute_status":     v.DisputeStatus,
			"dispute_amount":     v.DisputeAmount,
			"reason_code":        v.ReasonCode,
			"reason_description": v.ReasonDescription,
			"respond_by":         v.RespondBy,
		}
	default:
		m["data"] = map[string]any{
			"status": "unparsed_or_unknown",
		}
	}
	b, _ := json.Marshal(m)
	return b
}

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
	candidateSecrets := cashfree.OrderedCandidateSecrets(h.CashfreeSecret, h.CashfreeSecrets)
	if len(candidateSecrets) == 0 {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "payment gateway not configured"})
		return
	}

	sig := strings.TrimSpace(c.GetHeader("x-webhook-signature"))
	ts := strings.TrimSpace(c.GetHeader("x-webhook-timestamp"))
	if sig == "" || ts == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing signature or timestamp header"})
		return
	}

	validSig := false
	for _, sec := range candidateSecrets {
		if cashfree.VerifyWebhookHMAC(sec, ts, string(raw), sig) {
			validSig = true
			break
		}
	}
	if !validSig {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}

	// Parse verified payload before idempotency and drift verification
	val, evtType, parseErr := cashfree.ParseWebhook(raw)
	var allowedPayload []byte
	if parseErr != nil {
		allowedPayload = []byte(fmt.Sprintf(`{"error":"unparseable_payload","len":%d}`, len(raw)))
		evtType = "UNPARSEABLE"
	} else {
		allowedPayload = buildAllowedPaymentAuditPayload(evtType, val)
	}

	// 1. Check x-idempotency-header (and fallback x-idempotency-key) duplicate delivery.
	// We check idempotency BEFORE timestamp drift verification.
	// If this delivery is an authentic retry of an already-processed event,
	// acknowledge with HTTP 200 OK immediately to stop gateway retry storms.
	idempotencyHeader := strings.TrimSpace(c.GetHeader("x-idempotency-header"))
	if idempotencyHeader == "" {
		idempotencyHeader = strings.TrimSpace(c.GetHeader("x-idempotency-key"))
	}
	if idempotencyHeader != "" && h.GatewayPaymentRepo != nil {
		alreadyProcessed, err := h.GatewayPaymentRepo.HasProcessedEvent(c.Request.Context(), "cashfree", "WEBHOOK_IDEMPOTENCY", idempotencyHeader, "")
		if err == nil && alreadyProcessed {
			slog.Info("cashfree payment webhook duplicate delivery suppressed by x-idempotency-header", "idempotency_header", idempotencyHeader)
			c.Status(http.StatusOK)
			return
		}
	}

	// 2. Check natural key deduplication for already-settled payments, refunds, and disputes.
	// If the gateway retries an event after 5+ minutes (Attempt 3: +10m, Attempt 4: +30m)
	// that was already settled, return HTTP 200 OK to acknowledge and terminate retries.
	if h.GatewayPaymentRepo != nil && parseErr == nil {
		switch v := val.(type) {
		case cashfree.SuccessWebhook:
			if strings.TrimSpace(v.CFPaymentID) != "" {
				alreadyProcessed, err := h.GatewayPaymentRepo.HasProcessedEvent(c.Request.Context(), "cashfree", "PAYMENT_SETTLED", strings.TrimSpace(v.CFPaymentID), "")
				if err == nil && alreadyProcessed {
					slog.Info("cashfree payment webhook duplicate delivery suppressed by cf_payment_id", "cf_payment_id", v.CFPaymentID)
					c.Status(http.StatusOK)
					return
				}
			}
		case cashfree.RefundWebhook:
			refID := strings.TrimSpace(v.CFRefundID)
			if refID == "" {
				refID = strings.TrimSpace(v.RefundID)
			}
			if refID != "" {
				alreadyProcessed, err := h.GatewayPaymentRepo.HasProcessedEvent(c.Request.Context(), "cashfree", "REFUND_STATUS", refID, "")
				if err == nil && alreadyProcessed {
					slog.Info("cashfree refund webhook duplicate delivery suppressed by refund_id", "refund_id", refID)
					c.Status(http.StatusOK)
					return
				}
			}
		case cashfree.DisputeWebhook:
			if strings.TrimSpace(v.DisputeID) != "" {
				alreadyProcessed, err := h.GatewayPaymentRepo.HasProcessedEvent(c.Request.Context(), "cashfree", "DISPUTE_STATUS", strings.TrimSpace(v.DisputeID), "")
				if err == nil && alreadyProcessed {
					slog.Info("cashfree dispute webhook duplicate delivery suppressed by dispute_id", "dispute_id", v.DisputeID)
					c.Status(http.StatusOK)
					return
				}
			}
		}
	}

	// 3. Verify timestamp tolerance (drift check) with fail-closed default (300s).
	// Stale unhandled events outside tolerance window are rejected with 401, unless
	// live gateway API can independently verify that the payment or refund is genuine.
	tolerance := h.WebhookToleranceSec
	if tolerance <= 0 {
		tolerance = 300
	}
	if err := cashfree.VerifyWebhookTimestamp(ts, tolerance, time.Now().UTC()); err != nil {
		// FALLBACK GATEWAY VERIFICATION:
		// When Cashfree retries an unhandled event beyond tolerance (e.g. Attempt 3 at +10m,
		// Attempt 4 at +30m, or manual dashboard batch replay), verify with Cashfree's live REST API.
		verifiedByGateway := false
		if h.CashfreeClient != nil && parseErr == nil {
			switch v := val.(type) {
			case cashfree.SuccessWebhook:
				if strings.TrimSpace(v.OrderID) != "" {
					cfID, _, amount, ok, fErr := h.CashfreeClient.FetchSuccessfulPayment(c.Request.Context(), v.OrderID)
					cleanCFID := strings.TrimSpace(cfID)
					cleanWebhookID := strings.TrimSpace(v.CFPaymentID)
					if fErr == nil && ok && cleanCFID != "" && cleanWebhookID != "" && cleanCFID == cleanWebhookID && amount == v.AmountPaise {
						verifiedByGateway = true
						slog.Info("cashfree payment webhook timestamp drift verified via live gateway API", "order_id", v.OrderID, "cf_payment_id", cleanCFID)
					} else if fErr == nil && ok {
						slog.Warn("cashfree payment webhook fallback rejected: gateway payment does not match webhook",
							"order_id", v.OrderID,
							"webhook_cf_payment_id", cleanWebhookID,
							"gateway_cf_payment_id", cleanCFID,
							"webhook_amount", v.AmountPaise,
							"gateway_amount", amount,
						)
					}
				}
			case cashfree.RefundWebhook:
				refID := strings.TrimSpace(v.CFRefundID)
				if refID == "" {
					refID = strings.TrimSpace(v.RefundID)
				}
				if refID != "" && strings.TrimSpace(v.OrderID) != "" {
					details, fErr := h.CashfreeClient.FetchRefundStatus(c.Request.Context(), v.OrderID, refID)
					if fErr == nil && details != nil && details.RefundStatus == v.RefundStatus {
						verifiedByGateway = true
						slog.Info("cashfree refund webhook timestamp drift verified via live gateway API", "order_id", v.OrderID, "refund_id", refID)
					}
				}
			}
		}

		if !verifiedByGateway {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "timestamp tolerance exceeded"})
			return
		}
	}

	// Ingest allow-listed arrival into webhook_events audit log
	evtRecord := &domain.WebhookEvent{
		Provider:         "cashfree",
		EventType:        evtType,
		Signature:        &sig,
		TimestampHeader:  &ts,
		RawPayload:       allowedPayload,
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

	if parseErr != nil {
		// DEAD-LETTER ISOLATION: mark error, alert owner, return 200 OK to stop gateway retry storms
		errMsg := parseErr.Error()
		if evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(c.Request.Context(), evtRecord.ID, "dead_letter", &errMsg)
		}
		slog.Default().Error("cashfree webhook dead-letter (unparseable payload)", "err", parseErr)
		c.Status(http.StatusOK)
		return
	}

	switch evtType {
	case "PAYMENT_SUCCESS_WEBHOOK":
		succ, ok := val.(cashfree.SuccessWebhook)
		if !ok {
			c.Status(http.StatusOK)
			return
		}
		h.handlePaymentSuccessWebhook(c, succ, evtRecord, idempotencyHeader)

	case "PAYMENT_FAILED_WEBHOOK":
		fail, ok := val.(cashfree.FailedWebhook)
		if !ok {
			c.Status(http.StatusOK)
			return
		}
		h.handlePaymentFailedWebhook(c, fail, evtRecord, idempotencyHeader)

	case "REFUND_STATUS_WEBHOOK", "AUTO_REFUND_STATUS_WEBHOOK":
		ref, ok := val.(cashfree.RefundWebhook)
		if !ok {
			c.Status(http.StatusOK)
			return
		}
		h.handleRefundWebhook(c, ref, evtRecord, idempotencyHeader)

	case "DISPUTE_CREATED", "DISPUTE_CREATED_WEBHOOK", "PAYMENT_DISPUTE_CREATED", "PAYMENT_DISPUTE_CREATED_WEBHOOK", "DISPUTE_STATUS_UPDATE", "DISPUTE_STATUS_UPDATE_WEBHOOK":
		disp, ok := val.(cashfree.DisputeWebhook)
		if !ok {
			c.Status(http.StatusOK)
			return
		}
		h.handleDisputeWebhook(c, disp, evtRecord, idempotencyHeader)

	default:
		if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(c.Request.Context(), evtRecord.ID, "ignored", nil)
		}
		c.Status(http.StatusOK)
	}
}

func (h *Handlers) handlePaymentSuccessWebhook(c *gin.Context, succ cashfree.SuccessWebhook, evtRecord *domain.WebhookEvent, idempotencyHeader string) {
	ctx := c.Request.Context()
	// R7: Fail closed with HTTP 503 when the intent store is unavailable, so the provider retries.
	if h.IntentStore == nil {
		slog.Error("ALERT: payment success webhook received but IntentStore is unconfigured/unavailable", "order_id", succ.OrderID)
		c.AbortWithStatus(http.StatusServiceUnavailable)
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

	if idempotencyHeader != "" && h.GatewayPaymentRepo != nil {
		_, _ = h.GatewayPaymentRepo.RecordProcessedEvent(ctx, "cashfree", "WEBHOOK_IDEMPOTENCY", idempotencyHeader, "")
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

func (h *Handlers) handlePaymentFailedWebhook(c *gin.Context, fail cashfree.FailedWebhook, evtRecord *domain.WebhookEvent, idempotencyHeader string) {
	ctx := c.Request.Context()
	if h.Pool == nil || h.GatewayPaymentRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "persistence unavailable"})
		return
	}

	if txErr := postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
		txPayRepo := postgres.NewPaymentRepo(tx)
		firstSeen, err := txPayRepo.RecordProcessedEvent(ctx, "cashfree", "PAYMENT_FAILED_WEBHOOK", fail.CFPaymentID, "")
		if err != nil || !firstSeen {
			return err
		}
		// A failed ATTEMPT is not a failed ORDER: Cashfree lets the payer retry on the same
		// order until order_expiry_time. Leave the intent status untouched so a later
		// PAYMENT_SUCCESS_WEBHOOK or TenantDuePayVerify still finds it via LatestOpenForDue.
		// Terminal order failure/expiry is set by the stale-intent poller, not by this event.
		txIntentRepo := postgres.NewPaymentIntentRepo(tx)
		if err := txIntentRepo.TouchNonTerminalByOrderID(ctx, fail.OrderID); err != nil {
			return err
		}
		if idempotencyHeader != "" {
			_, _ = txPayRepo.RecordProcessedEvent(ctx, "cashfree", "WEBHOOK_IDEMPOTENCY", idempotencyHeader, "")
		}
		return nil
	}); txErr != nil {
		slog.Default().Error("PAYMENT_FAILED_WEBHOOK: transaction error updating payment_intents",
			"order_id", fail.OrderID,
			"cf_payment_id", fail.CFPaymentID,
			"err", txErr,
		)
		if evtRecord != nil && evtRecord.ID != uuid.Nil {
			errMsg := txErr.Error()
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "failed", &errMsg)
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to process webhook transaction"})
		return
	}

	if evtRecord != nil && evtRecord.ID != uuid.Nil {
		if err := h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "processed", nil); err != nil {
			slog.Default().Error("PAYMENT_FAILED_WEBHOOK: failed to update audit event status",
				"order_id", fail.OrderID,
				"cf_payment_id", fail.CFPaymentID,
				"err", err,
			)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update audit event status"})
			return
		}
	}
	c.Status(http.StatusOK)
}

func (h *Handlers) handleRefundWebhook(c *gin.Context, ref cashfree.RefundWebhook, evtRecord *domain.WebhookEvent, idempotencyHeader string) {
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

	if idempotencyHeader != "" && h.GatewayPaymentRepo != nil {
		_, _ = h.GatewayPaymentRepo.RecordProcessedEvent(ctx, "cashfree", "WEBHOOK_IDEMPOTENCY", idempotencyHeader, "")
	}

	if evtRecord.ID != uuid.Nil {
		_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "processed", nil)
	}
	c.Status(http.StatusOK)
}

func (h *Handlers) handleDisputeWebhook(c *gin.Context, disp cashfree.DisputeWebhook, evtRecord *domain.WebhookEvent, idempotencyHeader string) {
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

	if idempotencyHeader != "" && h.GatewayPaymentRepo != nil {
		_, _ = h.GatewayPaymentRepo.RecordProcessedEvent(ctx, "cashfree", "WEBHOOK_IDEMPOTENCY", idempotencyHeader, "")
	}

	// Fail-Safe Ledger Invariant: A dispute is a contested claim, NOT an authorized or confirmed refund.
	// We strictly refrain from mutating double-entry journals or resetting dues to pending automatically.
	// Returning HTTP 200 OK acknowledges the webhook delivery to prevent gateway retry storms.
	c.Status(http.StatusOK)
}
