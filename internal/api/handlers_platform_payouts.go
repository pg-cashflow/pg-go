package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func (h *Handlers) getPayoutPlatformStore() PayoutPlatformStore {
	if h.PayoutPlatformRepo != nil {
		return h.PayoutPlatformRepo
	}
	repo := h.getPayoutRepo()
	if repo != nil {
		return repo
	}
	return nil
}

// PlatformListPayoutExceptions handles GET /api/platform/payouts/exceptions.
func (h *Handlers) PlatformListPayoutExceptions(c *gin.Context) {
	limit := 50
	offset := 0
	if l := c.Query("limit"); l != "" {
		if val, err := strconv.Atoi(l); err == nil && val > 0 {
			limit = val
		}
	}
	if o := c.Query("offset"); o != "" {
		if val, err := strconv.Atoi(o); err == nil && val >= 0 {
			offset = val
		}
	}

	store := h.getPayoutPlatformStore()
	if store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout store not configured"})
		return
	}

	exceptions, err := store.ListPayoutExceptions(c.Request.Context(), limit, offset)
	if err != nil {
		respondErr(c, err)
		return
	}
	if exceptions == nil {
		exceptions = []domain.PayoutExceptionItem{}
	}
	c.JSON(http.StatusOK, gin.H{"exceptions": exceptions})
}

// PlatformListPayoutBatches handles GET /api/platform/payouts/batches.
func (h *Handlers) PlatformListPayoutBatches(c *gin.Context) {
	limit := 50
	offset := 0
	if l := c.Query("limit"); l != "" {
		if val, err := strconv.Atoi(l); err == nil && val > 0 {
			limit = val
		}
	}
	if o := c.Query("offset"); o != "" {
		if val, err := strconv.Atoi(o); err == nil && val >= 0 {
			offset = val
		}
	}

	var statusPtr *domain.PayoutBatchStatus
	if st := strings.TrimSpace(c.Query("status")); st != "" {
		s := domain.PayoutBatchStatus(st)
		statusPtr = &s
	}

	store := h.getPayoutPlatformStore()
	if store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout store not configured"})
		return
	}

	batches, err := store.ListAllPayoutBatches(c.Request.Context(), statusPtr, limit, offset)
	if err != nil {
		respondErr(c, err)
		return
	}
	if batches == nil {
		batches = []domain.PayoutBatch{}
	}
	c.JSON(http.StatusOK, gin.H{"batches": batches})
}

// PlatformGetPayoutBatch handles GET /api/platform/payouts/batches/:id.
func (h *Handlers) PlatformGetPayoutBatch(c *gin.Context) {
	batchID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	store := h.getPayoutPlatformStore()
	if store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout store not configured"})
		return
	}

	batch, err := store.GetBatchByIDCrossProperty(c.Request.Context(), batchID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "batch not found"})
			return
		}
		respondErr(c, err)
		return
	}

	items, err := store.ListPayoutItemsByBatchCrossProperty(c.Request.Context(), batchID)
	if err != nil {
		respondErr(c, err)
		return
	}
	if items == nil {
		items = []domain.PayoutItem{}
	}

	c.JSON(http.StatusOK, gin.H{
		"batch": batch,
		"items": items,
	})
}

type platformReconcilePayoutItemRequest struct {
	Status        string  `json:"status" binding:"required"`
	UTR           *string `json:"utr"`
	CFTransferID  *string `json:"cf_transfer_id"`
	SettledAt     *string `json:"settled_at"`
	FailureReason *string `json:"failure_reason"`
	Notes         string  `json:"notes" binding:"required"`
}

// PlatformReconcilePayoutItem handles POST /api/platform/payouts/items/:id/reconcile.
// DEPRECATED: Direct single-operator manual reconciliation is rejected to enforce maker-checker dual control.
// Operators must submit a case via POST /api/platform/payouts/items/:id/reconcile-case and obtain
// independent checker approval via POST /api/platform/payouts/reconcile-cases/:id/approve.
func (h *Handlers) PlatformReconcilePayoutItem(c *gin.Context) {
	c.JSON(http.StatusForbidden, gin.H{
		"error": "direct single-operator reconciliation is disabled: maker-checker two-person control is required (submit case to POST /api/platform/payouts/items/:id/reconcile-case)",
	})
}

type platformCreateReconciliationCaseRequest struct {
	ProposedStatus string  `json:"proposed_status" binding:"required"`
	UTR            *string `json:"utr"`
	CFTransferID   *string `json:"cf_transfer_id"`
	SettledAt      *string `json:"settled_at"`
	FailureReason  *string `json:"failure_reason"`
	Notes          string  `json:"notes" binding:"required"`
	EvidenceRef    string  `json:"evidence_ref" binding:"required"`
	EvidenceHash   string  `json:"evidence_hash" binding:"required"`
}

// PlatformCreatePayoutReconciliationCase handles POST /api/platform/payouts/items/:id/reconcile-case.
// Maker submits a proposed reconciliation with evidence. Payout item remains in its original status.
func (h *Handlers) PlatformCreatePayoutReconciliationCase(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims == nil {
		apierr.Abort(c, http.StatusUnauthorized, "unauthorized", apierr.CodeAuthUnauthorized)
		return
	}
	makerID := claims.UserID
	if makerID == uuid.Nil {
		apierr.Abort(c, http.StatusForbidden, "maker identity required", apierr.CodeAuthForbidden)
		return
	}

	itemID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	var body platformCreateReconciliationCaseRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body", "details": err.Error()})
		return
	}

	trimmedNotes := strings.TrimSpace(body.Notes)
	if trimmedNotes == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reconciliation notes are required"})
		return
	}
	trimmedEvidenceRef := strings.TrimSpace(body.EvidenceRef)
	if trimmedEvidenceRef == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "evidence_ref is required"})
		return
	}
	trimmedEvidenceHash := strings.TrimSpace(body.EvidenceHash)
	if trimmedEvidenceHash == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "evidence_hash is required"})
		return
	}

	newStatus := domain.PayoutItemStatus(strings.TrimSpace(body.ProposedStatus))
	switch newStatus {
	case domain.PayoutSucceeded:
		if body.UTR == nil || strings.TrimSpace(*body.UTR) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "utr is required when proposed status is succeeded"})
			return
		}
	case domain.PayoutFailed, domain.PayoutRejected:
		if body.FailureReason == nil || strings.TrimSpace(*body.FailureReason) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failure_reason is required when proposed status is failed or rejected"})
			return
		}
	case domain.PayoutReversed:
		// Reversal allowed
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid proposed reconciliation status (must be succeeded, failed, rejected, or reversed)"})
		return
	}

	var settledAtTime *time.Time
	if body.SettledAt != nil && strings.TrimSpace(*body.SettledAt) != "" {
		parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(*body.SettledAt))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid settled_at format (RFC3339 required)"})
			return
		}
		settledAtTime = &parsed
	}

	store := h.getPayoutPlatformStore()
	if store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout store not configured"})
		return
	}

	reconCase, err := store.CreatePayoutReconciliationCase(c.Request.Context(), domain.CreatePayoutReconciliationCaseParams{
		PayoutItemID:   itemID,
		ProposedStatus: newStatus,
		UTR:            body.UTR,
		CFTransferID:   body.CFTransferID,
		SettledAt:      settledAtTime,
		FailureReason:  body.FailureReason,
		Notes:          trimmedNotes,
		EvidenceRef:    trimmedEvidenceRef,
		EvidenceHash:   trimmedEvidenceHash,
		MakerID:        makerID,
	})
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "payout item not found"})
			return
		}
		if strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "cannot transition") || strings.Contains(err.Error(), "required") {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusCreated, gin.H{"case": reconCase})
}

// PlatformListPayoutReconciliationCases handles GET /api/platform/payouts/reconcile-cases.
func (h *Handlers) PlatformListPayoutReconciliationCases(c *gin.Context) {
	limit := 50
	offset := 0
	if l := c.Query("limit"); l != "" {
		if val, err := strconv.Atoi(l); err == nil && val > 0 {
			limit = val
		}
	}
	if o := c.Query("offset"); o != "" {
		if val, err := strconv.Atoi(o); err == nil && val >= 0 {
			offset = val
		}
	}
	var statusFilter *domain.PayoutReconciliationCaseStatus
	if st := strings.TrimSpace(c.Query("status")); st != "" {
		s := domain.PayoutReconciliationCaseStatus(st)
		statusFilter = &s
	}

	store := h.getPayoutPlatformStore()
	if store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout store not configured"})
		return
	}

	cases, err := store.ListPayoutReconciliationCases(c.Request.Context(), statusFilter, limit, offset)
	if err != nil {
		respondErr(c, err)
		return
	}

	if cases == nil {
		cases = []domain.PlatformPayoutReconciliationCase{}
	}
	c.JSON(http.StatusOK, gin.H{"cases": cases})
}

// PlatformGetPayoutReconciliationCase handles GET /api/platform/payouts/reconcile-cases/:id.
func (h *Handlers) PlatformGetPayoutReconciliationCase(c *gin.Context) {
	caseID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	store := h.getPayoutPlatformStore()
	if store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout store not configured"})
		return
	}

	reconCase, err := store.GetPayoutReconciliationCase(c.Request.Context(), caseID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "reconciliation case not found"})
			return
		}
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"case": reconCase})
}

type platformApproveReconciliationCaseRequest struct {
	Notes string `json:"notes"`
}

// PlatformApprovePayoutReconciliationCase handles POST /api/platform/payouts/reconcile-cases/:id/approve.
// Checker approves proposed case. Checker MUST be a distinct operator from Maker.
func (h *Handlers) PlatformApprovePayoutReconciliationCase(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims == nil {
		apierr.Abort(c, http.StatusUnauthorized, "unauthorized", apierr.CodeAuthUnauthorized)
		return
	}
	checkerID := claims.UserID
	if checkerID == uuid.Nil {
		apierr.Abort(c, http.StatusForbidden, "checker identity required", apierr.CodeAuthForbidden)
		return
	}

	caseID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	var body platformApproveReconciliationCaseRequest
	_ = c.ShouldBindJSON(&body)

	store := h.getPayoutPlatformStore()
	if store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout store not configured"})
		return
	}

	item, err := store.ApprovePayoutReconciliationCase(c.Request.Context(), caseID, checkerID, strings.TrimSpace(body.Notes))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "reconciliation case not found"})
			return
		}
		if strings.Contains(err.Error(), "maker cannot approve own reconciliation case") {
			c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
			return
		}
		if strings.Contains(err.Error(), "cannot approve") || strings.Contains(err.Error(), "cannot transition") {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"item": item})
}

type platformRejectReconciliationCaseRequest struct {
	Reason string `json:"reason" binding:"required"`
}

// PlatformRejectPayoutReconciliationCase handles POST /api/platform/payouts/reconcile-cases/:id/reject.
// Checker rejects proposed case with reason.
func (h *Handlers) PlatformRejectPayoutReconciliationCase(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims == nil {
		apierr.Abort(c, http.StatusUnauthorized, "unauthorized", apierr.CodeAuthUnauthorized)
		return
	}
	checkerID := claims.UserID
	if checkerID == uuid.Nil {
		apierr.Abort(c, http.StatusForbidden, "checker identity required", apierr.CodeAuthForbidden)
		return
	}

	caseID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	var body platformRejectReconciliationCaseRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body", "details": err.Error()})
		return
	}

	trimmedReason := strings.TrimSpace(body.Reason)
	if trimmedReason == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "rejection reason is required"})
		return
	}

	store := h.getPayoutPlatformStore()
	if store == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payout store not configured"})
		return
	}

	reconCase, err := store.RejectPayoutReconciliationCase(c.Request.Context(), caseID, checkerID, trimmedReason)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "reconciliation case not found"})
			return
		}
		if strings.Contains(err.Error(), "maker cannot reject own reconciliation case") {
			c.JSON(http.StatusForbidden, gin.H{"error": err.Error()})
			return
		}
		if strings.Contains(err.Error(), "cannot reject") {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"case": reconCase})
}
