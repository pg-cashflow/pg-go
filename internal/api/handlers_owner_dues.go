package api

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/billing"
	"github.com/pg-cashflow/pg-go/internal/collector"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/magiclink"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/qr"
)

func (h *Handlers) ListDues(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	limit := 50
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 50 {
			limit = n
		}
	}
	if off := c.Query("offset"); off != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "offset pagination is unsupported; use keyset cursor (cursor_due_date, cursor_id)"})
		return
	}
	f := postgres.DueListFilter{PropertyID: pid, Limit: limit}
	if cd := c.Query("cursor_due_date"); cd != "" {
		if t, err := time.Parse(time.RFC3339, cd); err == nil {
			f.CursorDueDate = &t
		}
	}
	if cid := c.Query("cursor_id"); cid != "" {
		if u, err := uuid.Parse(cid); err == nil {
			f.CursorID = &u
		}
	}
	if tid := c.Query("tenant_id"); tid != "" {
		id, err := uuid.Parse(tid)
		if err == nil {
			f.TenantID = &id
		}
	}
	if k := c.Query("kind"); k != "" {
		kind := domain.DueKind(k)
		f.Kind = &kind
	}
	if s := c.Query("status"); s != "" {
		st := domain.DueStatus(s)
		f.Status = &st
	}
	list, err := h.DueStore.List(c.Request.Context(), f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	if len(list) > limit {
		list = list[:limit]
	}
	resp := gin.H{"dues": list, "limit": limit}
	if len(list) == limit {
		last := list[len(list)-1]
		resp["next_cursor"] = gin.H{
			"cursor_due_date": last.DueDate.Format(time.RFC3339),
			"cursor_id":       last.ID,
		}
	}
	c.JSON(http.StatusOK, resp)
}

// WaiveDue handles POST /owner/dues/:id/waive.
func (h *Handlers) WaiveDue(c *gin.Context) {
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
	due, err := h.DueStore.GetByID(c.Request.Context(), id)
	if err != nil || due.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	var body struct {
		StepUpAuthInput
	}
	_ = c.ShouldBindJSON(&body)
	if _, ok := h.verifyDualControlOrStepUp(c, pid, uid, nil, body.StepUpAuthInput); !ok {
		return
	}
	due, err = h.Billing.WaiveDue(c.Request.Context(), id)
	if err != nil {
		respondErr(c, typedClientErr(http.StatusBadRequest, err, billing.ErrDueNotWaivable, billing.ErrNoOpenRentDue, billing.ErrOpenDueExists))
		return
	}
	c.JSON(http.StatusOK, due)
}

// ManualMatch handles POST /owner/dues/:id/match.
func (h *Handlers) ManualMatch(c *gin.Context) {
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
	due, err := h.DueStore.GetByID(c.Request.Context(), id)
	if err != nil || due.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	var body struct {
		AmountPaise int64  `json:"amount" binding:"required"`
		TxnID       string `json:"upi_txn_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "amount and upi_txn_id required"})
		return
	}
	if body.AmountPaise <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "amount must be strictly positive"})
		return
	}
	p, err := h.Payments.ManualMatch(c.Request.Context(), id, body.AmountPaise, body.TxnID, uid)
	if err != nil {
		respondErr(c, paymentClientErr(err))
		return
	}
	c.JSON(http.StatusOK, p)
}

// MarkCashPaid handles POST /owner/dues/:id/mark-cash-paid.
func (h *Handlers) MarkCashPaid(c *gin.Context) {
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
	due, err := h.DueStore.GetByID(c.Request.Context(), id)
	if err != nil || due.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	var body struct {
		AmountPaise int64  `json:"amount" binding:"required"`
		Note        string `json:"note"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "amount required"})
		return
	}
	p, err := h.Payments.MarkCashPaid(c.Request.Context(), id, body.AmountPaise, uid, body.Note)
	if err != nil {
		respondErr(c, paymentClientErr(err))
		return
	}
	c.JSON(http.StatusOK, p)
}

// DueQR handles GET /owner/dues/:id/qr.
func (h *Handlers) DueQR(c *gin.Context) {
	h.serveDueQR(c, true)
}

func (h *Handlers) serveDueQR(c *gin.Context, ownerScoped bool) {
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	due, err := h.DueStore.GetByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if ownerScoped {
		pid, ok := propertyIDFromClaims(c)
		if !ok {
			return
		}
		if due.PropertyID != pid {
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
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
	role := "tenant"
	if ownerScoped {
		role = "owner"
	}
	if h.Collector != nil {
		intent, png, err := h.Collector.PayIntent(c.Request.Context(), due, prop, room, collector.PNGURL(role, due.ID), phone)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "qr"})
			return
		}
		if intent.Mode == domain.PaymentModeCashfree {
			c.JSON(http.StatusConflict, gin.H{
				"error":              "personal UPI QR replaced by Cashfree checkout",
				"mode":               intent.Mode,
				"payment_session_id": intent.PaymentSessionID,
			})
			return
		}
		if len(png) == 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "not payable"})
			return
		}
		c.Data(http.StatusOK, "image/png", png)
		return
	}
	upi := qr.GenerateUPILink(prop.UPIVPA, prop.OwnerName, int64(due.Amount), due.DueCode, room)
	png, err := qr.GenerateQR(upi)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "qr"})
		return
	}
	c.Data(http.StatusOK, "image/png", png)
}

// DueToken handles POST /owner/dues/:id/token.
func (h *Handlers) DueToken(c *gin.Context) {
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
	path, err := h.MagicLink.CreatePaymentToken(c.Request.Context(), id)
	if err != nil {
		respondErr(c, err)
		return
	}
	url := h.MagicLinkBaseURL + path
	tenant, _ := h.TenantStore.GetByID(c.Request.Context(), due.TenantID)
	var wa string
	if tenant != nil {
		rupees := due.Amount / 100
		msg := "Pay rent ₹" + strconv.FormatInt(rupees, 10) + " — " + url
		wa = magiclink.BuildWALink(tenant.Phone, msg)
	}
	c.JSON(http.StatusOK, gin.H{"path": path, "url": url, "wa_me": wa})
}
