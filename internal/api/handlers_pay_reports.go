package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

const maxReportImage = 2 << 20

type reportBody struct {
	UPITxnID string `json:"upi_txn_id" binding:"required"`
	Amount   int64  `json:"amount"`
	Note     string `json:"note"`
}

// TenantSubmitReport handles POST /tenant/dues/:id/reports (multipart optional image, or JSON).
func (h *Handlers) TenantSubmitReport(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
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
	due, err := h.DueStore.GetByID(c.Request.Context(), id)
	if err != nil || due.TenantID != t.ID {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if due.Status == domain.DueStatusPaid || due.Status == domain.DueStatusWaived {
		c.JSON(http.StatusConflict, gin.H{"error": "already recorded"})
		return
	}

	var txnID string
	var amount int64
	var note string
	var img []byte
	ct := c.ContentType()
	if strings.HasPrefix(ct, "multipart/") {
		txnID = strings.TrimSpace(c.PostForm("upi_txn_id"))
		amount = atoi64(c.PostForm("amount"))
		note = c.PostForm("note")
		if f, err := c.FormFile("image"); err == nil && f != nil {
			if f.Size > maxReportImage {
				apierr.RespondClientErr(c, http.StatusBadRequest, "image too large (max 2MB)", apierr.CodeRequestImageTooLarge)
				return
			}
			rc, err := f.Open()
			if err == nil {
				img, _ = io.ReadAll(io.LimitReader(rc, maxReportImage+1))
				_ = rc.Close()
			}
			if len(img) > maxReportImage {
				apierr.RespondClientErr(c, http.StatusBadRequest, "image too large (max 2MB)", apierr.CodeRequestImageTooLarge)
				return
			}
			if len(img) > 0 && !allowedReportImage(img) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "image must be jpeg, png, or webp"})
				return
			}
		}
	} else {
		var body reportBody
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "upi_txn_id required"})
			return
		}
		txnID = strings.TrimSpace(body.UPITxnID)
		amount = body.Amount
		note = body.Note
	}
	if txnID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "upi_txn_id required"})
		return
	}
	if amount <= 0 {
		amount = due.Amount
	}
	if existing, err := h.ReportStore.GetByUPITxnID(c.Request.Context(), txnID); err == nil && existing != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "duplicate utr"})
		return
	} else if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}
	if h.PaymentLookup != nil {
		if p, err := h.PaymentLookup.GetByUPITxnID(c.Request.Context(), txnID); err == nil && p != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "duplicate utr"})
			return
		}
	}

	var imgHash *string
	var isDuplicate bool
	if len(img) > 0 {
		hSum := sha256.Sum256(img)
		hexHash := hex.EncodeToString(hSum[:])
		imgHash = &hexHash
		if dup, err := h.ReportStore.HasImageWithHash(c.Request.Context(), due.PropertyID, hexHash); err == nil && dup {
			isDuplicate = true
		}
	}

	rep := &domain.PaymentReport{
		DueID:       due.ID,
		TenantID:    t.ID,
		PropertyID:  due.PropertyID,
		UPITxnID:    txnID,
		Amount:      amount,
		ImageBytes:  img,
		ImageHash:   imgHash,
		IsDuplicate: isDuplicate,
		Status:      domain.ReportPendingReview,
		ReportedBy:  uid,
	}
	if note != "" {
		rep.Note = &note
	}
	if err := h.ReportStore.Create(c.Request.Context(), rep); err != nil {
		respondErr(c, err)
		return
	}
	rep.HasImage = len(img) > 0
	if h.Events != nil {
		payload, _ := json.Marshal(map[string]string{"report_id": rep.ID.String(), "upi_txn_id": txnID})
		did := due.ID
		_ = h.Events.Publish(c.Request.Context(), domain.Event{
			TenantID:   domain.Ptr(t.ID),
			PropertyID: due.PropertyID,
			EventType:  domain.EvtPaymentReportSubmitted,
			DueID:      &did,
			OccurredAt: rep.CreatedAt,
			Payload:    payload,
		})
	}
	if h.OutboxEvents != nil {
		tid := t.ID
		payload, _ := json.Marshal(map[string]string{"report_id": rep.ID.String()})
		_ = h.OutboxEvents.InsertEvent(c.Request.Context(), &domain.OutboxEvent{
			EventType:  string(domain.EvtPaymentReportSubmitted),
			PropertyID: due.PropertyID,
			TenantID:   &tid,
			ActorRole:  string(domain.RoleTenant),
			Payload:    payload,
		})
	}
	c.JSON(http.StatusCreated, tenantReportResponse(rep))
}

func tenantReportResponse(p *domain.PaymentReport) gin.H {
	return gin.H{
		"id":          p.ID,
		"due_id":      p.DueID,
		"tenant_id":   p.TenantID,
		"property_id": p.PropertyID,
		"upi_txn_id":  p.UPITxnID,
		"amount":      p.Amount,
		"has_image":   p.HasImage,
		"status":      p.Status,
		"reported_by": p.ReportedBy,
		"note":        p.Note,
		"created_at":  p.CreatedAt,
	}
}

func atoi64(s string) int64 {
	var n int64
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int64(r-'0')
	}
	return n
}

func allowedReportImage(b []byte) bool {
	switch http.DetectContentType(b) {
	case "image/jpeg", "image/png", "image/webp":
		return true
	default:
		return false
	}
}

// ListPaymentReports handles GET /owner/payment-reports.
func (h *Handlers) ListPaymentReports(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	var st *domain.PaymentReportStatus
	if q := c.Query("status"); q != "" {
		s := domain.PaymentReportStatus(q)
		st = &s
	}
	list, err := h.ReportStore.ListByProperty(c.Request.Context(), pid, st)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"payment_reports": list})
}

// ConfirmPaymentReport handles POST /owner/payment-reports/:id/confirm.
func (h *Handlers) ConfirmPaymentReport(c *gin.Context) {
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

	var rep *domain.PaymentReport
	var p *domain.Payment

	confirmOp := func(ctx context.Context, store ReportStore) error {
		r, err := store.GetByIDForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if r.PropertyID != pid {
			return domain.ErrNotFound
		}
		rep = r
		if rep.Status != domain.ReportPendingReview {
			return nil
		}
		matched, err := h.Payments.ManualMatch(ctx, rep.DueID, rep.Amount, rep.UPITxnID, uid)
		if err != nil {
			return err
		}
		p = matched
		now := p.MatchedAt
		rep.Status = domain.ReportConfirmed
		rep.ReviewedBy = &uid
		rep.ReviewedAt = &now
		return store.UpdateReview(ctx, rep)
	}

	if h.Pool != nil {
		err := postgres.WithinTx(c.Request.Context(), h.Pool, func(tx pgx.Tx) error {
			txStore := postgres.NewPaymentReportRepo(tx)
			return confirmOp(postgres.ContextWithTx(c.Request.Context(), tx), txStore)
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, domain.ErrNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
				return
			}
			respondErr(c, paymentClientErr(err))
			return
		}
	} else {
		if err := confirmOp(c.Request.Context(), h.ReportStore); err != nil {
			if errors.Is(err, pgx.ErrNoRows) || errors.Is(err, domain.ErrNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
				return
			}
			respondErr(c, paymentClientErr(err))
			return
		}
	}

	if rep == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if rep.Status != domain.ReportConfirmed {
		c.JSON(http.StatusOK, rep)
		return
	}
	c.JSON(http.StatusOK, gin.H{"report": rep, "payment": p})
}

// RejectPaymentReport handles POST /owner/payment-reports/:id/reject.
func (h *Handlers) RejectPaymentReport(c *gin.Context) {
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
	rep, err := h.ReportStore.GetByID(c.Request.Context(), id)
	if err != nil || rep.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if rep.Status != domain.ReportPendingReview {
		if rep.Status == domain.ReportConfirmed {
			c.JSON(http.StatusConflict, gin.H{"error": "already recorded"})
			return
		}
		c.JSON(http.StatusOK, rep)
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	_ = c.ShouldBindJSON(&body)
	now := time.Now().UTC()
	rep.Status = domain.ReportRejected
	rep.ReviewedBy = &uid
	rep.ReviewedAt = &now
	if body.Note != "" {
		rep.Note = &body.Note
	}
	_ = h.ReportStore.UpdateReview(c.Request.Context(), rep)
	if h.Events != nil {
		payload, _ := json.Marshal(map[string]string{"report_id": rep.ID.String()})
		did := rep.DueID
		_ = h.Events.Publish(c.Request.Context(), domain.Event{
			TenantID:   domain.Ptr(rep.TenantID),
			PropertyID: pid,
			EventType:  domain.EvtPaymentReportRejected,
			DueID:      &did,
			OccurredAt: now,
			Payload:    payload,
		})
	}
	if h.OutboxEvents != nil {
		tid := rep.TenantID
		payload, _ := json.Marshal(map[string]string{"report_id": rep.ID.String()})
		_ = h.OutboxEvents.InsertEvent(c.Request.Context(), &domain.OutboxEvent{
			EventType:  string(domain.EvtPaymentReportRejected),
			PropertyID: pid,
			TenantID:   &tid,
			ActorRole:  string(domain.RoleOwner),
			Payload:    payload,
		})
	}
	c.JSON(http.StatusOK, rep)
}

