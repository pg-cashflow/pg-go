package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/collector"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

func (h *Handlers) duePayJSON(c *gin.Context, due *domain.Due, role string) {
	if due.Status == domain.DueStatusPaid || due.Status == domain.DueStatusWaived {
		c.JSON(http.StatusOK, domain.PayIntent{
			Mode:        domain.PaymentModeManual,
			Note:        domain.UPINote(due.DueCode),
			DueCode:     due.DueCode,
			AmountPaise: due.Amount,
			QRPNGURL:    collector.PNGURL(role, due.ID),
			Payable:     false,
		})
		return
	}
	prop, err := h.PropertyStore.GetByID(c.Request.Context(), due.PropertyID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "property"})
		return
	}
	room, phone := "", ""
	if t, err := h.TenantStore.GetByID(c.Request.Context(), due.TenantID); err == nil {
		if t.RoomNumber != nil {
			room = *t.RoomNumber
		}
		if t.Phone != nil {
			phone = *t.Phone
		}
	}
	if h.Collector == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "collector"})
		return
	}
	intent, _, err := h.Collector.PayIntent(c.Request.Context(), due, prop, room, collector.PNGURL(role, due.ID), phone)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "pay intent failed"})
		return
	}
	c.JSON(http.StatusOK, intent)
}

// OwnerDuePay handles GET /owner/dues/:id/pay.
func (h *Handlers) OwnerDuePay(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	due, err := h.DueStore.GetByID(c.Request.Context(), id)
	if err != nil || due.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	h.duePayJSON(c, due, "owner")
}

// TenantDuePay handles GET /tenant/dues/:id/pay.
func (h *Handlers) TenantDuePay(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	if !t.IsPayable() {
		c.JSON(http.StatusOK, domain.PayIntent{
			Mode:    domain.PaymentModeManual,
			Payable: false,
		})
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
	h.duePayJSON(c, due, "tenant")
}

const maxReportImage = 2 << 20

type reportBody struct {
	UPITxnID string `json:"upi_txn_id" binding:"required"`
	Amount   int    `json:"amount"`
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
	var amount int
	var note string
	var img []byte
	ct := c.ContentType()
	if strings.HasPrefix(ct, "multipart/") {
		txnID = strings.TrimSpace(c.PostForm("upi_txn_id"))
		amount = atoi(c.PostForm("amount"))
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

	rep := &domain.PaymentReport{
		DueID:      due.ID,
		TenantID:   t.ID,
		PropertyID: due.PropertyID,
		UPITxnID:   txnID,
		Amount:     amount,
		ImageBytes: img,
		Status:     domain.ReportPendingReview,
		ReportedBy: uid,
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
	c.JSON(http.StatusCreated, rep)
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return n
		}
		n = n*10 + int(r-'0')
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
	rep, err := h.ReportStore.GetByID(c.Request.Context(), id)
	if err != nil || rep.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if rep.Status != domain.ReportPendingReview {
		c.JSON(http.StatusOK, rep)
		return
	}
	p, err := h.Payments.ManualMatch(c.Request.Context(), rep.DueID, rep.Amount, rep.UPITxnID, uid)
	if err != nil {
		respondErr(c, paymentClientErr(err))
		return
	}
	now := p.MatchedAt
	rep.Status = domain.ReportConfirmed
	rep.ReviewedBy = &uid
	rep.ReviewedAt = &now
	_ = h.ReportStore.UpdateReview(c.Request.Context(), rep)
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
	// ADR-2 H1 defense-in-depth: when no secret is configured but IntentStore
	// is wired, Cashfree is partially configured — refuse rather than silently
	// accepting unauthenticated webhooks. Only skip when Cashfree is fully absent.
	if h.CashfreeSecret == "" {
		if h.IntentStore != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "payment gateway not configured"})
			return
		}
		c.Status(http.StatusOK)
		return
	}
	sig := c.GetHeader("x-webhook-signature")
	ts := c.GetHeader("x-webhook-timestamp")
	if !cashfree.VerifyWebhookHMAC(h.CashfreeSecret, ts, string(raw), sig) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}
	evt, ok, err := cashfree.ParseSuccessWebhook(raw)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "json"})
		return
	}
	if !ok {
		c.Status(http.StatusOK)
		return
	}
	if h.IntentStore == nil {
		c.Status(http.StatusOK)
		return
	}
	intent, err := h.IntentStore.GetByOrderID(c.Request.Context(), evt.OrderID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			c.Status(http.StatusOK)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}
	if evt.AmountPaise != intent.AmountPaise {
		c.Status(http.StatusOK)
		return
	}
	if evt.CFPaymentID != "" {
		if existing, err := h.IntentStore.GetByCFPaymentID(c.Request.Context(), evt.CFPaymentID); err == nil && existing != nil {
			c.Status(http.StatusOK)
			return
		}
	}
	txn := evt.TxnID()
	_, err = h.Payments.GatewaySettle(c.Request.Context(), intent.DueID, evt.AmountPaise, txn)
	if err != nil && !errors.Is(err, payment.ErrDuplicateTxn) {
		if errors.Is(err, payment.ErrDueNotOpen) || errors.Is(err, payment.ErrEmptyTxnID) {
			c.Status(http.StatusOK)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settle failed"})
		return
	}
	if evt.CFPaymentID != "" {
		_ = h.IntentStore.MarkPaid(c.Request.Context(), intent.ID, evt.CFPaymentID)
	}
	c.Status(http.StatusOK)
}
