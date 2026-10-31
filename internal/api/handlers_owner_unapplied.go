package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
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
	receiptID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	if h.UnmatchedReceipts == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "unmatched receipts repository unavailable"})
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
	if receipt.Status == domain.ReceiptStatusResolved {
		c.JSON(http.StatusBadRequest, gin.H{"error": "receipt is already resolved"})
		return
	}

	// 2. Cryptographic Step-Up Authentication / Dual Control Verification
	if req.StepUpAuth.Purpose == "" {
		req.StepUpAuth.Purpose = "unapplied_receipt_refund"
	}
	if _, ok := h.verifyDualControlOrStepUp(c, pid, uid, nil, req.StepUpAuth); !ok {
		return
	}

	ctx := c.Request.Context()

	// 3. Resolve payment ID. If no payment row exists (amount-mismatch path), create an unapplied payment row under transaction.
	var paymentID uuid.UUID
	if receipt.PaymentID != nil {
		paymentID = *receipt.PaymentID
	} else {
		if receipt.TenantID == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot refund receipt: tenant is not assigned"})
			return
		}
		paymentID = uuid.New()
		note := "unapplied receipt held awaiting refund"
		if req.Notes != "" {
			note = req.Notes
		}
		now := time.Now().UTC()
		var dueID *uuid.UUID = receipt.DueID
		newPayment := &domain.Payment{
			ID:                paymentID,
			PropertyID:        &pid,
			TenantID:          *receipt.TenantID,
			Amount:            receipt.AmountPaise,
			Provider:          receipt.Provider,
			ProviderPaymentID: &receipt.ProviderPaymentID,
			CFPaymentID:       &receipt.ProviderPaymentID,
			MatchedBy:         domain.MatchedByCashfree,
			RecordedBy:        &uid,
			MatchedAt:         now,
			IsUnapplied:       true,
			RawNote:           &note,
			CreatedAt:         now,
		}
		if dueID != nil {
			newPayment.DueID = *dueID
		}
		if err := h.GatewayPaymentRepo.Create(ctx, newPayment); err != nil {
			slog.Error("failed to create unapplied payment for refund", "receipt_id", receiptID, "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record unapplied payment for refund"})
			return
		}
		if err := h.UnmatchedReceipts.LinkUnmatchedReceiptPayment(ctx, receiptID, paymentID); err != nil {
			slog.Error("failed to link unapplied payment to receipt", "receipt_id", receiptID, "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to link payment to receipt"})
			return
		}
	}

	// 4. Mark receipt status 'resolving'
	_ = h.UnmatchedReceipts.MarkUnmatchedReceiptResolving(ctx, receiptID, idempotencyKey, req.Notes)

	// 5. Execute refund reusing established refund logic in handlers_pay_refunds.go
	reason := req.Reason
	if reason == "" {
		reason = "refund of unapplied held receipt"
	}

	refundReq := OwnerRefundRequest{
		AmountPaise:     receipt.AmountPaise,
		Reason:          reason,
		StepUpAuthInput: req.StepUpAuth,
	}

	// Set Idempotency-Key on context request header so executePaymentRefund sees it
	c.Request.Header.Set("Idempotency-Key", idempotencyKey)
	refRow, err := h.executePaymentRefund(c, pid, uid, paymentID, refundReq)
	if err != nil {
		return
	}

	if refRow != nil {
		_ = h.UnmatchedReceipts.LinkUnmatchedReceiptRefund(ctx, receiptID, refRow.ID)
	}
}
