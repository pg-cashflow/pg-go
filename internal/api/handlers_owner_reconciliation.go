package api

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

func (h *Handlers) ListPayments(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	var mb *domain.MatchedBy
	if s := c.Query("matched_by"); s != "" {
		m := domain.MatchedBy(s)
		mb = &m
	}
	limit := 50
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 50 {
			limit = n
		}
	}
	var before *time.Time
	var beforeID *uuid.UUID
	if cur := c.Query("cursor"); cur != "" {
		if t, err := time.Parse(time.RFC3339, cur); err == nil {
			before = &t
		}
	}
	if cid := c.Query("cursor_id"); cid != "" {
		if id, err := uuid.Parse(cid); err == nil {
			beforeID = &id
		}
	}
	if paginated, ok := h.PaymentStore.(interface {
		ListByPropertyPaginated(ctx context.Context, propertyID uuid.UUID, matchedBy *domain.MatchedBy, limit int, before *time.Time, beforeID *uuid.UUID) ([]domain.Payment, error)
	}); ok {
		list, err := paginated.ListByPropertyPaginated(c.Request.Context(), pid, mb, limit, before, beforeID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
			return
		}
		var nextCursor *string
		var nextCursorID *string
		if len(list) == limit {
			last := list[len(list)-1]
			ts := last.MatchedAt.Format(time.RFC3339)
			id := last.ID.String()
			nextCursor = &ts
			nextCursorID = &id
		}
		c.JSON(http.StatusOK, gin.H{"payments": list, "limit": limit, "next_cursor": nextCursor, "next_cursor_id": nextCursorID})
		return
	}

	list, err := h.PaymentStore.ListByProperty(c.Request.Context(), pid, mb)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	if len(list) > limit {
		list = list[:limit]
	}
	c.JSON(http.StatusOK, gin.H{"payments": list, "limit": limit})
}

// ListEvents handles GET /owner/events (Requirement 11 pagination, max 50).
func (h *Handlers) ListEvents(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	f := postgres.EventFilter{PropertyID: pid}
	if tid := c.Query("tenant_id"); tid != "" {
		id, err := uuid.Parse(tid)
		if err == nil {
			f.TenantID = &id
		}
	}
	if t := c.Query("type"); t != "" {
		et := domain.EventType(t)
		f.EventType = &et
	}
	if from := c.Query("from"); from != "" {
		if tm, err := time.Parse("2006-01-02", from); err == nil {
			f.From = &tm
		}
	}
	if to := c.Query("to"); to != "" {
		if tm, err := time.Parse("2006-01-02", to); err == nil {
			f.To = &tm
		}
	}
	limit := 50
	if lim := c.Query("limit"); lim != "" {
		if n, err := strconv.Atoi(lim); err == nil && n > 0 && n <= 50 {
			limit = n
		}
	}
	f.Limit = limit
	list, err := h.EventStore.List(c.Request.Context(), f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	// Hide internal bigserial ids in JSON via domain.Event `json:"-"` on ID.
	c.JSON(http.StatusOK, gin.H{"events": list, "limit": limit})
}

type OwnerVerifyPaymentInput struct {
	DueID       uuid.UUID `json:"due_id" binding:"required"`
	AmountPaise int64     `json:"amount_paise" binding:"required"`
	UPITxnID    string    `json:"upi_txn_id" binding:"required"`
	Note        string    `json:"note"`
}

// OwnerVerifyPayment handles POST /owner/payments/verify (Requirements 4, 5, 18).
// Atomic, idempotent server-side payment verification.
func (h *Handlers) OwnerVerifyPayment(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	var in OwnerVerifyPaymentInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "due_id, amount_paise, and upi_txn_id required"})
		return
	}
	if in.AmountPaise <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "amount_paise must be greater than zero"})
		return
	}
	normUTR, err := domain.NormalizeUTR(in.UPITxnID)
	if err != nil {
		respondErr(c, clientErr(http.StatusBadRequest, "invalid upi_txn_id: "+err.Error()))
		return
	}
	p, err := h.Payments.VerifyPayment(c.Request.Context(), payment.VerifyPaymentInput{
		PropertyID:  pid,
		DueID:       in.DueID,
		AmountPaise: in.AmountPaise,
		UTR:         normUTR,
		RecordedBy:  uid,
		Note:        in.Note,
	})
	if err != nil {
		respondErr(c, paymentClientErr(err))
		return
	}
	c.JSON(http.StatusOK, p)
}

type OwnerCorrectPaymentInput struct {
	CorrectedAmount int64  `json:"corrected_amount_paise" binding:"required"`
	CorrectedUTR    string `json:"corrected_upi_txn_id"`
	Reason          string `json:"reason" binding:"required"`
}

// OwnerCorrectPayment handles POST /owner/payments/:id/correct (Requirement 16).
// Creates reversing entry and corrected entry while maintaining an immutable audit log.
func (h *Handlers) OwnerCorrectPayment(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	var in OwnerCorrectPaymentInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "corrected_amount_paise and reason required"})
		return
	}
	corr, err := h.Payments.CorrectPayment(c.Request.Context(), payment.CorrectPaymentInput{
		PropertyID:      pid,
		PaymentID:       id,
		CorrectedAmount: in.CorrectedAmount,
		CorrectedUTR:    in.CorrectedUTR,
		Reason:          in.Reason,
		CorrectedBy:     uid,
	})
	if err != nil {
		respondErr(c, paymentClientErr(err))
		return
	}
	c.JSON(http.StatusOK, corr)
}

// Reconciliation handles GET /owner/reconciliation.
func (h *Handlers) Reconciliation(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	period := c.Query("period")
	if period == "" {
		period = time.Now().Format("2006-01")
	}
	sum, err := h.Payments.BuildSummary(c.Request.Context(), pid, period)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, sum)
}
