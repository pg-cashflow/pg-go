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
func (h *Handlers) PlatformReconcilePayoutItem(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims == nil {
		apierr.Abort(c, http.StatusUnauthorized, "unauthorized", apierr.CodeAuthUnauthorized)
		return
	}
	operatorID := claims.UserID
	if operatorID == uuid.Nil {
		apierr.Abort(c, http.StatusForbidden, "operator identity required", apierr.CodeAuthForbidden)
		return
	}

	itemID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	var body platformReconcilePayoutItemRequest
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body", "details": err.Error()})
		return
	}

	trimmedNotes := strings.TrimSpace(body.Notes)
	if trimmedNotes == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reconciliation notes are required"})
		return
	}

	newStatus := domain.PayoutItemStatus(strings.TrimSpace(body.Status))
	switch newStatus {
	case domain.PayoutSucceeded:
		if body.UTR == nil || strings.TrimSpace(*body.UTR) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "utr is required when status is succeeded"})
			return
		}
	case domain.PayoutFailed, domain.PayoutRejected:
		if body.FailureReason == nil || strings.TrimSpace(*body.FailureReason) == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "failure_reason is required when status is failed or rejected"})
			return
		}
	case domain.PayoutReversed:
		// Reversal allowed
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid reconciliation status (must be succeeded, failed, rejected, or reversed)"})
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

	updatedItem, err := store.ReconcilePayoutItemManual(
		c.Request.Context(),
		itemID,
		newStatus,
		body.CFTransferID,
		body.UTR,
		settledAtTime,
		body.FailureReason,
		operatorID,
		trimmedNotes,
	)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "payout item not found"})
			return
		}
		if strings.Contains(err.Error(), "cannot transition") || strings.Contains(err.Error(), "required") {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		respondErr(c, err)
		return
	}

	c.JSON(http.StatusOK, gin.H{"item": updatedItem})
}
