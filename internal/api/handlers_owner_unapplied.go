package api

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/text/unicode/norm"

	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// ResolveReceiptCreditReq payload for converting an unapplied receipt to tenant advance credit.
type ResolveReceiptCreditReq struct {
	IdempotencyKey string          `json:"idempotency_key"`
	Notes          string          `json:"notes,omitempty"`
	StepUpAuth     StepUpAuthInput `json:"step_up_auth"`
}

// ResolveReceiptRefundReq payload for refunding an unapplied receipt back to source.
type ResolveReceiptRefundReq struct {
	IdempotencyKey string          `json:"idempotency_key"`
	Reason         string          `json:"reason,omitempty"`
	Notes          string          `json:"notes,omitempty"`
	StepUpAuth     StepUpAuthInput `json:"step_up_auth"`
}

// OwnerListUnappliedReceipts handles GET /owner/payments/unapplied-receipts.
func (h *Handlers) OwnerListUnappliedReceipts(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if h.UnmatchedReceipts == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "unmatched receipts repository unavailable"})
		return
	}

	statusFilter := strings.TrimSpace(c.Query("status"))
	list, err := h.UnmatchedReceipts.ListUnmatchedReceipts(c.Request.Context(), pid, statusFilter)
	if err != nil {
		slog.Error("failed to list unmatched receipts", "property_id", pid, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list unapplied receipts"})
		return
	}
	if list == nil {
		list = []domain.UnmatchedGatewayReceipt{}
	}

	c.JSON(http.StatusOK, gin.H{
		"unapplied_receipts": list,
		"receipts":           list,
		"count":              len(list),
	})
}

// OwnerGetUnappliedReceipt handles GET /owner/payments/unapplied-receipts/:id.
func (h *Handlers) OwnerGetUnappliedReceipt(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	receiptID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	if h.UnmatchedReceipts == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "unmatched receipts repository unavailable"})
		return
	}

	receipt, err := h.UnmatchedReceipts.GetUnmatchedReceiptByID(c.Request.Context(), receiptID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "unapplied receipt not found"})
			return
		}
		slog.Error("failed to get unmatched receipt", "id", receiptID, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to get unapplied receipt"})
		return
	}

	if receipt.PropertyID != nil && *receipt.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "unapplied receipt not found for property"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"unapplied_receipt": receipt,
	})
}

// OwnerApplyUnappliedReceiptCredit handles POST /owner/payments/unapplied-receipts/:id/credit.
func (h *Handlers) OwnerApplyUnappliedReceiptCredit(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	receiptID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	if h.UnmatchedReceipts == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "unmatched receipts repository unavailable"})
		return
	}

	var req ResolveReceiptCreditReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid resolution payload"})
		return
	}

	idempotencyKey := strings.TrimSpace(req.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	}
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(c.GetHeader("X-Idempotency-Key"))
	}
	if idempotencyKey == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "idempotency_key or Idempotency-Key header is required"})
		return
	}

	// 1. Verify receipt exists and belongs to this property
	receipt, err := h.UnmatchedReceipts.GetUnmatchedReceiptByID(c.Request.Context(), receiptID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "unapplied receipt not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query receipt"})
		return
	}
	if receipt.PropertyID != nil && *receipt.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "unapplied receipt not found for property"})
		return
	}
	if !receipt.IsResolvable() {
		if receipt.Status == domain.ReceiptStatusResolved &&
			receipt.ResolutionIdempotencyKey != nil && *receipt.ResolutionIdempotencyKey == idempotencyKey {
			c.JSON(http.StatusOK, gin.H{
				"status":  "resolved",
				"action":  receipt.ResolutionAction,
				"receipt": receipt,
			})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("receipt is not resolvable; current status: %s", receipt.Status)})
		return
	}

	// 2. Cryptographic Step-Up Authentication / Dual Control Verification
	if req.StepUpAuth.Purpose == "" {
		req.StepUpAuth.Purpose = "unapplied_receipt_resolution"
	}
	if _, ok := h.verifyDualControlOrStepUp(c, pid, uid, nil, req.StepUpAuth); !ok {
		return
	}

	// 3. Atomically apply credit to tenant and mark receipt resolved
	p, err := h.UnmatchedReceipts.ResolveUnmatchedReceiptAsCredit(c.Request.Context(), receiptID, uid, idempotencyKey, req.Notes)
	if err != nil {
		slog.Error("failed to resolve unmatched receipt as credit", "receipt_id", receiptID, "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"status":  "resolved",
		"action":  domain.ReceiptActionCredit,
		"payment": p,
	})
}

// Bind unapplied receipt refund OTPs to the receipt UUID (24 chars: "ur" + 22 base64 chars).
// Fits in VARCHAR(32) and keeps payout/payment codes isolated.
func unappliedReceiptRefundPurpose(id uuid.UUID) string {
	return "ur" + base64.RawURLEncoding.EncodeToString(id[:])
}

// RequestUnappliedReceiptRefundOTP handles POST /owner/payments/unapplied-receipts/:id/refund/request-otp.
func (h *Handlers) RequestUnappliedReceiptRefundOTP(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	receiptID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	if h.UnmatchedReceipts == nil || h.Auth == nil || h.UserStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "refund authorization unavailable"})
		return
	}
	receipt, err := h.UnmatchedReceipts.GetUnmatchedReceiptByID(c.Request.Context(), receiptID)
	if err != nil || receipt == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "unapplied receipt not found"})
		return
	}
	if receipt.PropertyID == nil || *receipt.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "unapplied receipt not found for property"})
		return
	}
	if receipt.Status != domain.ReceiptStatusHeld {
		c.JSON(http.StatusBadRequest, gin.H{"error": "receipt is not in held status"})
		return
	}
	user, err := h.UserStore.GetByID(c.Request.Context(), uid)
	if err != nil || user == nil || strings.TrimSpace(user.Phone) == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "registered phone required for refund authorization"})
		return
	}
	purpose := unappliedReceiptRefundPurpose(receiptID)
	channel, destination, err := h.Auth.RequestStepUpOTPWithChannel(c.Request.Context(), user.Phone, purpose, nil)
	if err != nil {
		if errors.Is(err, auth.ErrRateLimited) {
			respondErr(c, clientErrWithCode(http.StatusTooManyRequests, "please wait before requesting another code", apierr.CodeAuthRateLimited))
		} else if errors.Is(err, auth.ErrNoDeliveryChannel) {
			respondErr(c, clientErrWithCode(http.StatusUnprocessableEntity, "no delivery channel available", apierr.CodeAuthNoDeliveryChannel))
		} else {
			respondErr(c, err)
		}
		return
	}
	if channel == "sms" {
		destination = maskPhone(destination)
	}
	c.JSON(http.StatusOK, gin.H{"channel": channel, "destination": destination})
}

// OwnerRefundUnappliedReceipt handles POST /owner/payments/unapplied-receipts/:id/refund.
func (h *Handlers) OwnerRefundUnappliedReceipt(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	h.processRefundUnappliedReceipt(c, pid, uid, nil, false)
}

// ManagerRefundUnappliedReceipt handles POST /manager/payments/unapplied-receipts/:id/refund.
// It verifies that the property owner has enabled allow_manager_refunds in property settings.
func (h *Handlers) ManagerRefundUnappliedReceipt(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	limit, ok := h.requireManagerRefunds(c, pid)
	if !ok {
		return
	}
	h.processRefundUnappliedReceipt(c, pid, uid, limit, true)
}

func (h *Handlers) processRefundUnappliedReceipt(c *gin.Context, pid, uid uuid.UUID, managerLimit *int64, isManager bool) {
	receiptID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	if h.UnmatchedReceipts == nil || h.GatewayPaymentRepo == nil || h.Pool == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "refund service temporarily unavailable"})
		return
	}

	var req ResolveReceiptRefundReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid refund payload"})
		return
	}

	idempotencyKey := strings.TrimSpace(req.IdempotencyKey)
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	}
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(c.GetHeader("X-Idempotency-Key"))
	}
	if idempotencyKey == "" {
		apierr.RespondClientErr(c, http.StatusBadRequest, "Idempotency-Key header is required for refund operations", apierr.CodeFinanceIdempotencyRequired)
		return
	}
	if len(idempotencyKey) > 128 {
		apierr.RespondClientErr(c, http.StatusBadRequest, "Idempotency-Key header cannot exceed 128 characters", apierr.CodeRequestInvalidBody)
		return
	}

	ctx := c.Request.Context()

	// 1. Verify receipt exists and belongs to this property
	receipt, err := h.UnmatchedReceipts.GetUnmatchedReceiptByID(ctx, receiptID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "unapplied receipt not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to query receipt"})
		return
	}
	if receipt.PropertyID == nil || *receipt.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "unapplied receipt not found for property"})
		return
	}
	if receipt.Status == domain.ReceiptStatusResolved {
		c.JSON(http.StatusBadRequest, gin.H{"error": "receipt is already resolved"})
		return
	}
	if receipt.Status == "resolving" {
		if receipt.ResolutionIdempotencyKey != nil && *receipt.ResolutionIdempotencyKey == idempotencyKey && receipt.ResolutionRefundID != nil {
			if existingRef, lookupErr := h.GatewayPaymentRepo.GetRefundByID(ctx, *receipt.ResolutionRefundID); lookupErr == nil && existingRef != nil {
				c.JSON(http.StatusOK, existingRef)
				return
			}
			c.JSON(http.StatusAccepted, gin.H{
				"message": "refund already initiated; processing in progress",
			})
			return
		}
		c.JSON(http.StatusConflict, gin.H{
			"error": "receipt resolution already in progress with different idempotency key",
			"code":  "IDEMPOTENCY_CONFLICT",
		})
		return
	}
	if receipt.Status != domain.ReceiptStatusHeld {
		c.JSON(http.StatusBadRequest, gin.H{"error": "receipt is not in held status"})
		return
	}
	if receipt.TenantID == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot refund receipt: tenant is not assigned"})
		return
	}

	trimmedReason := norm.NFC.String(strings.TrimSpace(req.Reason))
	if trimmedReason == "" {
		trimmedReason = "refund of unapplied held receipt"
	}
	runeCount := utf8.RuneCountInString(trimmedReason)
	if runeCount < 3 || runeCount > 100 {
		apierr.RespondBindErr(c, "reason must be between 3 and 100 characters", apierr.CodeRefundReasonLength)
		return
	}
	if !validRefundReasonRegex.MatchString(trimmedReason) {
		apierr.RespondBindErr(c, "reason contains invalid characters", apierr.CodeRefundReasonCharacters)
		return
	}
	req.Reason = trimmedReason

	source := "owner"
	if isManager {
		source = "manager"
		if managerRefundExceedsLimit(managerLimit, receipt.AmountPaise) {
			apierr.RespondClientErr(c, http.StatusForbidden, "refund amount exceeds the limit set by the property owner; ask the owner to issue it", apierr.CodeRefundManagerLimitExceeded)
			return
		}
	}

	// Single-owner threshold evaluation for high-value refunds
	effectiveLimit := h.getRefundSingleOwnerThreshold(ctx, pid)
	_, singleOwnerAllowed := domain.EvaluateRefundThreshold(receipt.AmountPaise, effectiveLimit, h.AppEnv)
	if !singleOwnerAllowed {
		apierr.RespondClientErr(c, http.StatusForbidden, "refund exceeds single-owner authorization limit; dual owner approval required via refund approval requests", apierr.CodeRefundDualControlRequired)
		return
	}

	// 2. Unconditionally bind exact receipt purpose and verify OTP once
	req.StepUpAuth.Purpose = unappliedReceiptRefundPurpose(receiptID)
	req.StepUpAuth.BatchID = nil
	if _, ok := h.verifyDualControlOrStepUp(c, pid, uid, nil, req.StepUpAuth); !ok {
		return
	}

	// 3. Single transactional unit of work
	var rfRow *domain.GatewayRefund
	var paymentRecord *domain.Payment

	txErr := postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
		// AUTH-05 Lock Order:
		// 1. Manager Capability check (if manager)
		if isManager {
			if err := postgres.AssertCapabilityTx(ctx, tx, pid, uid, "manager", "refunds.request"); err != nil {
				return err
			}
		}

		// 2. Lock Unmatched Receipt row FOR UPDATE
		var lockedStatus string
		var lockedPid *uuid.UUID
		var lockedTenantID *uuid.UUID
		var lockedPaymentID *uuid.UUID
		var lockedAmount int64
		var lockedProvider string
		var lockedProviderPaymentID string
		err := tx.QueryRow(ctx, `
			SELECT status, property_id, tenant_id, payment_id, amount_paise, provider, provider_payment_id
			FROM unmatched_gateway_receipts
			WHERE id = $1 FOR UPDATE`, receiptID,
		).Scan(&lockedStatus, &lockedPid, &lockedTenantID, &lockedPaymentID, &lockedAmount, &lockedProvider, &lockedProviderPaymentID)
		if err != nil {
			return fmt.Errorf("lock receipt: %w", err)
		}
		if lockedPid == nil || *lockedPid != pid {
			return fmt.Errorf("receipt property mismatch")
		}
		if lockedStatus != string(domain.ReceiptStatusHeld) {
			return fmt.Errorf("receipt status is %s; cannot refund", lockedStatus)
		}
		if lockedTenantID == nil {
			return fmt.Errorf("receipt tenant is not assigned")
		}

		txPaymentRepo := postgres.NewPaymentRepo(tx)

		// 3. Adopt or create payment using transaction-backed PaymentRepo
		var p *domain.Payment
		if lockedPaymentID != nil {
			existingP, pErr := txPaymentRepo.GetByID(ctx, *lockedPaymentID)
			if pErr != nil || existingP == nil {
				return fmt.Errorf("linked payment not found: %w", pErr)
			}
			if existingP.PropertyID == nil || *existingP.PropertyID != pid ||
				existingP.TenantID != *lockedTenantID ||
				existingP.Provider != lockedProvider ||
				int64(existingP.Amount) != lockedAmount ||
				!existingP.IsUnapplied {
				return fmt.Errorf("linked payment identity mismatch against receipt")
			}
			p = existingP
		} else {
			// Check if payment already exists with this provider payment ID
			existingP, pErr := txPaymentRepo.GetByProviderPaymentID(ctx, lockedProviderPaymentID)
			if pErr == nil && existingP != nil {
				if existingP.PropertyID == nil || *existingP.PropertyID != pid ||
					existingP.TenantID != *lockedTenantID ||
					existingP.Provider != lockedProvider ||
					int64(existingP.Amount) != lockedAmount ||
					!existingP.IsUnapplied {
					return fmt.Errorf("existing payment identity mismatch against receipt")
				}
				p = existingP
				if _, linkErr := tx.Exec(ctx, `UPDATE unmatched_gateway_receipts SET payment_id = $1 WHERE id = $2`, p.ID, receiptID); linkErr != nil {
					return fmt.Errorf("link adopted payment: %w", linkErr)
				}
			} else {
				paymentID := uuid.New()
				now := time.Now().UTC()
				note := "unapplied receipt held awaiting refund"
				if req.Notes != "" {
					note = req.Notes
				}
				newPayment := &domain.Payment{
					ID:                paymentID,
					PropertyID:        &pid,
					TenantID:          *lockedTenantID,
					Amount:            lockedAmount,
					Provider:          lockedProvider,
					ProviderPaymentID: &lockedProviderPaymentID,
					CFPaymentID:       &lockedProviderPaymentID,
					MatchedBy:         domain.MatchedByCashfree,
					RecordedBy:        &uid,
					MatchedAt:         now,
					IsUnapplied:       true,
					SkipOutboxEnqueue: true, // Defer ledger mirror until after prepareRefundTx locks
					RawNote:           &note,
					CreatedAt:         now,
				}
				if receipt.DueID != nil {
					newPayment.DueID = *receipt.DueID
				}
				if err := txPaymentRepo.Create(ctx, newPayment); err != nil {
					return fmt.Errorf("create unapplied payment: %w", err)
				}
				if _, linkErr := tx.Exec(ctx, `UPDATE unmatched_gateway_receipts SET payment_id = $1 WHERE id = $2`, newPayment.ID, receiptID); linkErr != nil {
					return fmt.Errorf("link payment to receipt: %w", linkErr)
				}
				p = newPayment
			}
		}
		paymentRecord = p

		// 4. Mark receipt status 'resolving'
		res, err := tx.Exec(ctx, `
			UPDATE unmatched_gateway_receipts
			SET status = 'resolving',
			    resolution_idempotency_key = $1,
			    resolution_notes = $2,
			    updated_at = NOW()
			WHERE id = $3 AND status = 'held'`,
			idempotencyKey, req.Notes, receiptID)
		if err != nil {
			return fmt.Errorf("mark receipt resolving: %w", err)
		}
		if res.RowsAffected() == 0 {
			return fmt.Errorf("receipt is not in held status or was modified concurrently")
		}

		// 5. Resolve gateway order ID
		var orderID string
		if p.CFPaymentID != nil && *p.CFPaymentID != "" && h.IntentStore != nil {
			if intent, err := h.IntentStore.GetByCFPaymentID(ctx, *p.CFPaymentID); err == nil && intent != nil {
				orderID = intent.ProviderOrderID
			}
		}
		if orderID == "" && p.ProviderPaymentID != nil && *p.ProviderPaymentID != "" && h.IntentStore != nil {
			if intent, err := h.IntentStore.GetByCFPaymentID(ctx, *p.ProviderPaymentID); err == nil && intent != nil {
				orderID = intent.ProviderOrderID
			}
		}
		if orderID == "" && receipt.PaymentIntentID != nil {
			var intentOrderID string
			if err := tx.QueryRow(ctx, `SELECT provider_order_id FROM payment_intents WHERE id = $1`, *receipt.PaymentIntentID).Scan(&intentOrderID); err == nil && intentOrderID != "" {
				orderID = intentOrderID
			}
		}
		if orderID == "" && p.ProviderPaymentID != nil && *p.ProviderPaymentID != "" {
			var intentOrderID string
			if err := tx.QueryRow(ctx, `SELECT provider_order_id FROM payment_intents WHERE cf_payment_id = $1`, *p.ProviderPaymentID).Scan(&intentOrderID); err == nil && intentOrderID != "" {
				orderID = intentOrderID
			}
		}
		if orderID == "" {
			return fmt.Errorf("cannot resolve gateway order ID for unapplied receipt")
		}

		cleanUUID := strings.ReplaceAll(uuid.New().String(), "-", "")
		refundRef := fmt.Sprintf("rf%s", cleanUUID)
		now := time.Now().UTC()
		orderPayload, _ := json.Marshal(map[string]any{"order_id": orderID})

		rfRow = &domain.GatewayRefund{
			ID:              uuid.New(),
			PaymentID:       p.ID,
			PropertyID:      pid,
			Provider:        "cashfree",
			RefundReference: &refundRef,
			IdempotencyKey:  &idempotencyKey,
			AmountPaise:     lockedAmount,
			Status:          "initiated",
			Source:          source,
			InitiatedBy:     &uid,
			Reason:          trimmedReason,
			RawPayload:      orderPayload,
			CreatedAt:       now,
			UpdatedAt:       now,
		}

		refundReq := OwnerRefundRequest{
			AmountPaise: lockedAmount,
			Reason:      trimmedReason,
		}

		// 6. Execute refund preparation (locks tenant, dues, payment, allocations, gateway_refunds)
		if err := prepareRefundTx(ctx, tx, pid, p, refundReq, rfRow); err != nil {
			return fmt.Errorf("prepare refund: %w", err)
		}

		// 7. Enqueue deferred payment ledger outbox event now that payment locks have been taken
		mirrorPayload := domain.PaymentMirrorPayload{
			PropertyID:     pid,
			PaymentID:      p.ID,
			Allocations:    nil,
			UnappliedPaise: int64(p.Amount),
			IsUnapplied:    true,
			MatchedAt:      p.MatchedAt,
			MatchedBy:      p.MatchedBy,
			AmountPaise:    int64(p.Amount),
		}
		rawPayload, mErr := json.Marshal(mirrorPayload)
		if mErr != nil {
			return fmt.Errorf("marshal payment mirror outbox: %w", mErr)
		}
		outboxKey := fmt.Sprintf("payment_mirror:%s", p.ID)
		if _, oErr := tx.Exec(ctx, `
			INSERT INTO ledger_outbox_events (event_type, property_id, source_id, payload, idempotency_key, created_at)
			VALUES ($1, $2, $3, $4, $5, NOW())
			ON CONFLICT (idempotency_key) DO NOTHING`,
			domain.LedgerOutboxPayment, pid, p.ID, rawPayload, outboxKey); oErr != nil {
			return fmt.Errorf("enqueue deferred payment ledger outbox: %w", oErr)
		}

		// 8. Link resulting refund reference to receipt (immutable reference)
		resRef, err := tx.Exec(ctx, `
			UPDATE unmatched_gateway_receipts
			SET resolution_refund_id = $1, updated_at = NOW()
			WHERE id = $2`, rfRow.ID, receiptID)
		if err != nil {
			return fmt.Errorf("link refund to receipt: %w", err)
		}
		if resRef.RowsAffected() == 0 {
			return fmt.Errorf("failed to link refund to receipt")
		}

		return nil
	})

	if txErr != nil {
		slog.Error("unapplied receipt refund transaction failed", "receipt_id", receiptID, "err", txErr)
		if errors.Is(txErr, postgres.ErrCapabilityDenied) || errors.Is(txErr, postgres.ErrManagerInactive) {
			apierr.RespondClientErr(c, http.StatusForbidden, "manager lacks required permission to issue refunds", apierr.CodeFinanceForbidden)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to process refund transaction"})
		return
	}

	// Publish domain telemetry event
	if h.Events != nil && paymentRecord != nil && paymentRecord.TenantID != uuid.Nil && rfRow != nil {
		eventData, _ := json.Marshal(map[string]any{
			"payment_id":   paymentRecord.ID,
			"receipt_id":   receiptID,
			"refund_id":    rfRow.ID,
			"amount_paise": rfRow.AmountPaise,
			"reason":       rfRow.Reason,
			"actor_id":     uid,
		})
		_ = h.Events.Publish(ctx, domain.Event{
			PropertyID: pid,
			TenantID:   &paymentRecord.TenantID,
			EventType:  domain.EvtPaymentRefundPending,
			OccurredAt: rfRow.CreatedAt,
			Payload:    eventData,
		})
	}

	c.JSON(http.StatusAccepted, gin.H{
		"message": "refund initiated; processing via gateway outbox",
		"refund":  rfRow,
	})
}
