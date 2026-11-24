package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"golang.org/x/text/unicode/norm"
)

type CreateRefundApprovalRequestBody struct {
	AmountPaise int64  `json:"amount_paise" binding:"required,gt=0"`
	Reason      string `json:"reason" binding:"required,max=100"`
}

type ApproveRefundRequestBody struct {
	Note string `json:"note,omitempty"`
	StepUpAuthInput
}

type RejectRefundRequestBody struct {
	Reason string `json:"reason" binding:"required,max=200"`
}

// getRefundSingleOwnerThreshold resolves the effective single-owner refund authorization threshold.
// Checks property-level onboarding settings first; falls back to account-level configured default.
func (h *Handlers) getRefundSingleOwnerThreshold(ctx context.Context, pid uuid.UUID) *int64 {
	if h.OnboardingSettings != nil {
		if s, err := h.OnboardingSettings.Get(ctx, pid); err == nil && s != nil {
			if s.SingleOwnerRefundThresholdPaise != nil {
				return s.SingleOwnerRefundThresholdPaise
			}
		}
	}
	return h.SingleOwnerRefundThresholdPaise
}

// OwnerCreateRefundRequest handles POST /owner/payments/:id/refund/requests.
func (h *Handlers) OwnerCreateRefundRequest(c *gin.Context) {
	h.executeCreateRefundRequest(c, "owner")
}

// ManagerCreateRefundRequest handles POST /manager/payments/:id/refund/requests.
func (h *Handlers) ManagerCreateRefundRequest(c *gin.Context) {
	h.executeCreateRefundRequest(c, "manager")
}

func (h *Handlers) executeCreateRefundRequest(c *gin.Context, role string) {
	ctx := c.Request.Context()
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	paymentID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	var body CreateRefundApprovalRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "invalid request body: amount_paise must be positive and reason required", apierr.CodeRequestInvalidBody)
		return
	}

	trimmedReason := norm.NFC.String(strings.TrimSpace(body.Reason))
	if len(trimmedReason) < 3 || len(trimmedReason) > 100 {
		apierr.RespondBindErr(c, "reason must be between 3 and 100 characters", apierr.CodeRefundReasonLength)
		return
	}
	if !validRefundReasonRegex.MatchString(trimmedReason) {
		apierr.RespondBindErr(c, "reason contains invalid characters", apierr.CodeRefundReasonCharacters)
		return
	}
	body.Reason = trimmedReason

	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(c.GetHeader("X-Idempotency-Key"))
	}
	if idempotencyKey == "" {
		apierr.RespondClientErr(c, http.StatusBadRequest, "Idempotency-Key header is required for refund operations", apierr.CodeFinanceIdempotencyRequired)
		return
	}

	if h.GatewayPaymentRepo == nil || h.TenantStore == nil || h.RefundApprovalRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "refund approval repository unavailable"})
		return
	}

	// Verify payment and tenant
	p, err := h.GatewayPaymentRepo.GetByID(ctx, paymentID)
	if err != nil || p == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "payment not found"})
		return
	}
	tenant, err := h.TenantStore.GetByID(ctx, p.TenantID)
	if err != nil || tenant == nil || tenant.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "payment not found"})
		return
	}

	// Verify remaining refundable balance
	refundedPaise, err := h.GatewayPaymentRepo.GetPaymentRefundedPaise(ctx, paymentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to calculate refundable balance"})
		return
	}
	remaining := p.Amount - refundedPaise
	if body.AmountPaise > remaining {
		apierr.RespondClientErr(c, http.StatusBadRequest, "refund amount exceeds remaining refundable balance", apierr.CodePaymentRefundExceedsBalance)
		return
	}

	// Compute canonical material hash first for authoritative content binding
	materialHash := domain.ComputeRefundMaterialHash(pid, paymentID, p.TenantID, body.AmountPaise, body.Reason)

	// Check existing idempotency
	existing, err := h.RefundApprovalRepo.GetRequestByPaymentAndIdempotency(ctx, paymentID, idempotencyKey)
	if err == nil && existing != nil {
		if existing.AmountPaise != body.AmountPaise || existing.Reason != body.Reason || existing.MaterialHash != materialHash {
			c.JSON(http.StatusConflict, gin.H{
				"error": "idempotency key conflict: refund request exists with different amount or reason",
				"code":  "IDEMPOTENCY_CONFLICT",
			})
			return
		}
		c.JSON(http.StatusOK, gin.H{"refund_request": existing})
		return
	}

	// Evaluate policy threshold
	effectiveLimit := h.getRefundSingleOwnerThreshold(ctx, pid)
	requiredApprovals, _ := domain.EvaluateRefundThreshold(body.AmountPaise, effectiveLimit, h.AppEnv)

	req := &domain.RefundApprovalRequest{
		ID:                uuid.New(),
		PropertyID:        pid,
		PaymentID:         paymentID,
		TenantID:          p.TenantID,
		AmountPaise:       body.AmountPaise,
		Reason:            body.Reason,
		IdempotencyKey:    idempotencyKey,
		RequestedBy:       uid,
		RequestedByRole:   role,
		Status:            domain.RefundApprovalPending,
		RequiredApprovals: requiredApprovals,
		MaterialHash:      materialHash,
		Approvals:         []domain.RefundApprovalDecision{},
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
	}

	if err := h.RefundApprovalRepo.CreateRequest(ctx, req); err != nil {
		if errors.Is(err, postgres.ErrRefundApprovalConflict) {
			// Database-enforced atomic conflict recovery: read concurrently inserted row
			existing, getErr := h.RefundApprovalRepo.GetRequestByPaymentAndIdempotency(ctx, paymentID, idempotencyKey)
			if getErr == nil && existing != nil {
				if existing.AmountPaise == body.AmountPaise && existing.Reason == body.Reason && existing.MaterialHash == materialHash {
					c.JSON(http.StatusOK, gin.H{"refund_request": existing})
					return
				}
				c.JSON(http.StatusConflict, gin.H{
					"error": "idempotency key conflict: refund request exists with different parameters",
					"code":  "IDEMPOTENCY_CONFLICT",
				})
				return
			}
		}
		if errors.Is(err, postgres.ErrManagerRefundsDisabled) {
			apierr.RespondClientErr(c, http.StatusForbidden, "manager refunds are disabled by the property owner", apierr.CodeFinanceForbidden)
			return
		}
		if errors.Is(err, postgres.ErrManagerRefundLimitExceeded) {
			apierr.RespondClientErr(c, http.StatusForbidden, "refund amount exceeds the limit set by the property owner; ask the owner to issue it", apierr.CodeRefundManagerLimitExceeded)
			return
		}
		if errors.Is(err, postgres.ErrCapabilityDenied) || errors.Is(err, postgres.ErrManagerInactive) {
			apierr.RespondClientErr(c, http.StatusForbidden, "manager lacks required permission to issue refunds", apierr.CodeFinanceForbidden)
			return
		}
		slog.Error("failed to create refund approval request", "payment_id", paymentID, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record refund approval request"})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"refund_request": req})
}

// OwnerApproveRefundRequest handles POST /owner/refund-requests/:id/approve.
func (h *Handlers) OwnerApproveRefundRequest(c *gin.Context) {
	ctx := c.Request.Context()
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	requestID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims.Role != domain.RoleOwner {
		c.JSON(http.StatusForbidden, gin.H{"error": "only owners can approve refund requests", "code": "auth.forbidden"})
		return
	}

	var body ApproveRefundRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		body = ApproveRefundRequestBody{}
	}

	// Backend verification: User account must be active and not revoked
	if h.UserStore != nil {
		user, err := h.UserStore.GetByID(ctx, uid)
		if err != nil || user == nil {
			apierr.RespondClientErr(c, http.StatusUnauthorized, "owner account revoked or not found", apierr.CodeAuthAccessRevoked)
			return
		}
		if claims.TokenVersion > 0 && user.TokenVersion > claims.TokenVersion {
			apierr.RespondClientErr(c, http.StatusUnauthorized, "session revoked: token version stale", apierr.CodeAuthAccessRevoked)
			return
		}

		// Re-verify owner belongs to property
		owners, err := h.UserStore.GetByPropertyAndRole(ctx, pid, domain.RoleOwner)
		if err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "owner authorization lookup unavailable"})
			return
		}
		isPropertyOwner := false
		for _, o := range owners {
			if o.ID == uid {
				isPropertyOwner = true
				break
			}
		}
		if !isPropertyOwner {
			apierr.RespondClientErr(c, http.StatusForbidden, "user is not an active owner of this property", apierr.CodeAuthNoPropertyScope)
			return
		}
	}

	if h.RefundApprovalRepo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "refund approval repository unavailable"})
		return
	}

	req, err := h.RefundApprovalRepo.GetRequestByID(ctx, requestID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "refund approval request not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to fetch refund approval request"})
		return
	}

	// Cross-property check
	if req.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "refund approval request not found"})
		return
	}

	// 1. Maker cannot perform prohibited self-approval
	if uid == req.RequestedBy {
		apierr.RespondClientErr(c, http.StatusForbidden, "maker cannot approve own refund request; self-approval prohibited", apierr.CodeRefundMakerCannotApprove)
		return
	}

	// 2. Approval integrity and stale approval check: re-verify payment and tenant
	if h.GatewayPaymentRepo != nil && h.TenantStore != nil {
		p, err := h.GatewayPaymentRepo.GetByID(ctx, req.PaymentID)
		if err != nil || p == nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "payment not found"})
			return
		}
		tenant, err := h.TenantStore.GetByID(ctx, p.TenantID)
		if err != nil || tenant == nil || tenant.PropertyID != pid {
			c.JSON(http.StatusNotFound, gin.H{"error": "payment not found"})
			return
		}

		currentHash := domain.ComputeRefundMaterialHash(pid, req.PaymentID, tenant.ID, req.AmountPaise, req.Reason)
		if currentHash != req.MaterialHash {
			apierr.RespondClientErr(c, http.StatusConflict, "stale approval: material transaction details changed; previous approvals invalidated", apierr.CodeRefundStaleApproval)
			return
		}
	}

	// 3. Distinct approver check: one owner cannot satisfy both approvals
	for _, prev := range req.Approvals {
		if prev.ApproverID == uid {
			apierr.RespondClientErr(c, http.StatusConflict, "one owner cannot satisfy both approvals; distinct approver required", apierr.CodeRefundDuplicateApprover)
			return
		}
	}

	// 4. Session security: step-up re-authentication
	body.StepUpAuthInput.Purpose = paymentRefundPurpose(req.PaymentID)
	body.StepUpAuthInput.BatchID = nil
	mode, ok := h.verifyDualControlOrStepUp(c, pid, uid, &req.RequestedBy, body.StepUpAuthInput)
	if !ok {
		return
	}

	decision := domain.RefundApprovalDecision{
		ApproverID:   uid,
		ApproverRole: string(domain.RoleOwner),
		ApprovedAt:   time.Now().UTC(),
		StepUpMethod: mode,
		SnapshotHash: req.MaterialHash,
		Note:         body.Note,
	}

	// Transactional dispatch callback executed atomically when final approval commits
	onApproved := func(tx pgx.Tx, r *domain.RefundApprovalRequest) error {
		return h.dispatchApprovedRefundTx(ctx, tx, pid, uid, r)
	}

	requiredApprovals := req.RequiredApprovals
	effectiveLimit := h.getRefundSingleOwnerThreshold(ctx, pid)
	latestRequired, _ := domain.EvaluateRefundThreshold(req.AmountPaise, effectiveLimit, h.AppEnv)
	if latestRequired > requiredApprovals {
		requiredApprovals = latestRequired
	}

	updatedReq, err := h.RefundApprovalRepo.RecordApprovalAtomic(ctx, requestID, decision, requiredApprovals, onApproved)
	if err != nil {
		switch {
		case errors.Is(err, domain.ErrMakerCannotApprove):
			apierr.RespondClientErr(c, http.StatusForbidden, "maker cannot approve own refund request; self-approval prohibited", apierr.CodeRefundMakerCannotApprove)
			return
		case errors.Is(err, domain.ErrDuplicateApprover):
			apierr.RespondClientErr(c, http.StatusConflict, "one owner cannot satisfy both approvals; distinct approver required", apierr.CodeRefundDuplicateApprover)
			return
		case errors.Is(err, domain.ErrStaleApproval):
			apierr.RespondClientErr(c, http.StatusConflict, "stale approval: material transaction details changed; approval invalidated", apierr.CodeRefundStaleApproval)
			return
		case errors.Is(err, domain.ErrRefundRequestNotPending):
			apierr.RespondClientErr(c, http.StatusConflict, "refund approval request is not pending", apierr.CodeRequestInvalidBody)
			return
		default:
			slog.Error("failed to record refund approval", "request_id", requestID, "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record refund approval"})
			return
		}
	}

	slog.Info("refund approval recorded",
		"request_id", updatedReq.ID,
		"payment_id", updatedReq.PaymentID,
		"approver_id", uid,
		"status", updatedReq.Status,
		"approval_count", len(updatedReq.Approvals),
		"required_approvals", updatedReq.RequiredApprovals,
	)

	c.JSON(http.StatusOK, gin.H{"refund_request": updatedReq})
}

// OwnerRejectRefundRequest handles POST /owner/refund-requests/:id/reject.
func (h *Handlers) OwnerRejectRefundRequest(c *gin.Context) {
	ctx := c.Request.Context()
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	requestID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims.Role != domain.RoleOwner {
		c.JSON(http.StatusForbidden, gin.H{"error": "only owners can reject refund requests", "code": "auth.forbidden"})
		return
	}

	var body RejectRefundRequestBody
	if err := c.ShouldBindJSON(&body); err != nil {
		body.Reason = "Rejected by owner"
	}

	if h.RefundApprovalRepo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "refund approval repository unavailable"})
		return
	}

	req, err := h.RefundApprovalRepo.GetRequestByID(ctx, requestID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "refund approval request not found"})
		return
	}
	if req.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "refund approval request not found"})
		return
	}

	updatedReq, err := h.RefundApprovalRepo.RejectRequestAtomic(ctx, requestID, uid, body.Reason)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": fmt.Sprintf("cannot reject refund request: %v", err)})
		return
	}

	slog.Info("refund request rejected",
		"request_id", requestID,
		"rejected_by", uid,
		"reason", body.Reason,
	)

	c.JSON(http.StatusOK, gin.H{"refund_request": updatedReq})
}

// GetRefundRequest handles GET /owner/refund-requests/:id.
func (h *Handlers) GetRefundRequest(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	requestID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	if h.RefundApprovalRepo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "refund approval repository unavailable"})
		return
	}

	req, err := h.RefundApprovalRepo.GetRequestByID(c.Request.Context(), requestID)
	if err != nil || req == nil || req.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "refund approval request not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"refund_request": req})
}

// ListPaymentRefundRequests handles GET /owner/payments/:id/refund/requests.
func (h *Handlers) ListPaymentRefundRequests(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	paymentID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	if h.RefundApprovalRepo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "refund approval repository unavailable"})
		return
	}

	list, err := h.RefundApprovalRepo.ListRequestsByPayment(c.Request.Context(), paymentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list refund requests"})
		return
	}

	filtered := make([]domain.RefundApprovalRequest, 0, len(list))
	for _, item := range list {
		if item.PropertyID == pid {
			filtered = append(filtered, item)
		}
	}

	c.JSON(http.StatusOK, gin.H{"refund_requests": filtered})
}

// OwnerListRefundRequests handles GET /owner/refund-requests.
// Bounded to 200 records, property-scoped, allow-listed status filter.
func (h *Handlers) OwnerListRefundRequests(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}

	if h.RefundApprovalRepo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "refund approval repository unavailable"})
		return
	}

	statusFilter := strings.TrimSpace(c.Query("status"))
	if statusFilter != "" {
		switch statusFilter {
		case domain.RefundApprovalPending, domain.RefundApprovalPartiallyApproved,
			domain.RefundApprovalApproved, domain.RefundApprovalRejected,
			domain.RefundApprovalDispatched, domain.RefundApprovalCancelled:
			// valid
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status filter"})
			return
		}
	}

	list, err := h.RefundApprovalRepo.ListRequestsByProperty(c.Request.Context(), pid, statusFilter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list refund requests"})
		return
	}

	const maxLimit = 200
	if len(list) > maxLimit {
		list = list[:maxLimit]
	}

	c.JSON(http.StatusOK, gin.H{"refund_requests": list})
}

// dispatchApprovedRefundTx atomically writes the authorized refund record and outbox event
// inside the caller's database transaction using the authoritative preparation and allocation engine.
func (h *Handlers) dispatchApprovedRefundTx(ctx context.Context, tx pgx.Tx, pid, approverID uuid.UUID, r *domain.RefundApprovalRequest) error {
	rfID := uuid.New()
	r.RefundID = &rfID

	if tx == nil {
		return nil
	}

	txPayRepo := postgres.NewPaymentRepo(tx)
	p, err := txPayRepo.GetByID(ctx, r.PaymentID)
	if err != nil {
		return fmt.Errorf("fetch payment for approved refund dispatch: %w", err)
	}
	if p == nil {
		return fmt.Errorf("payment %s not found for approved refund dispatch", r.PaymentID)
	}

	cleanUUID := strings.ReplaceAll(uuid.New().String(), "-", "")
	refundRef := fmt.Sprintf("rf%s", cleanUUID)
	now := time.Now().UTC()
	idempKey := r.IdempotencyKey

	rfRow := &domain.GatewayRefund{
		ID:              rfID,
		PaymentID:       r.PaymentID,
		PropertyID:      pid,
		Provider:        "cashfree",
		RefundReference: &refundRef,
		IdempotencyKey:  &idempKey,
		AmountPaise:     r.AmountPaise,
		Status:          "initiated",
		Source:          "owner",
		InitiatedBy:     &r.RequestedBy,
		Reason:          r.Reason,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	ownerReq := OwnerRefundRequest{
		AmountPaise: r.AmountPaise,
		Reason:      r.Reason,
	}

	err = prepareRefundTx(ctx, tx, pid, p, ownerReq, rfRow)
	if err != nil {
		return fmt.Errorf("prepare and allocate approved refund: %w", err)
	}

	r.RefundID = &rfRow.ID
	return nil
}
