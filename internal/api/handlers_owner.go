package api

import (
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/billing"
	"github.com/pg-cashflow/pg-go/internal/collector"
	"github.com/pg-cashflow/pg-go/internal/csv"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/magiclink"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/qr"
)

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// ListProperties handles GET /owner/properties.
func (h *Handlers) ListProperties(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	// Property-scoped: return the owner's property (and any they can see via list filter).
	all, err := h.PropertyStore.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	out := make([]domain.Property, 0, 1)
	for _, p := range all {
		if p.ID == pid {
			p.UPIVPA = "" // never expose in API
			out = append(out, p)
		}
	}
	c.JSON(http.StatusOK, gin.H{"properties": out})
}

type createTenantBody struct {
	Name             string  `json:"name" binding:"required"`
	Phone            *string `json:"phone"`
	RoomNumber       *string `json:"room_number"`
	RentAmount       int     `json:"rent_amount" binding:"required"`
	DueDay           int16   `json:"due_day" binding:"required"`
	NoticePeriodDays int16   `json:"notice_period_days"`
	DepositAmount    *int    `json:"deposit_amount"`
}

// CreateTenant handles POST /owner/tenants.
func (h *Handlers) CreateTenant(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	var body createTenantBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if body.DueDay < 1 || body.DueDay > 28 {
		apierr.RespondClientErr(c, http.StatusBadRequest, "due_day must be 1–28", apierr.CodeRequestDueDayInvalid)
		return
	}
	if body.NoticePeriodDays <= 0 {
		body.NoticePeriodDays = 30
	}
	deposit := body.RentAmount
	if body.DepositAmount != nil {
		deposit = *body.DepositAmount
	}
	in := domain.NewTenantInput{
		PropertyID:       pid,
		Name:             body.Name,
		Phone:            body.Phone,
		RoomNumber:       body.RoomNumber,
		RentAmount:       body.RentAmount,
		DueDay:           body.DueDay,
		NoticePeriodDays: body.NoticePeriodDays,
	}
	t, err := h.Tenants.CreateTenant(c.Request.Context(), in, deposit)
	if err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusCreated, tenantResponse(t))
}

// ListTenants handles GET /owner/tenants.
func (h *Handlers) ListTenants(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	list, err := h.TenantStore.ListByProperty(c.Request.Context(), pid)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	out := make([]gin.H, 0, len(list))
	for i := range list {
		out = append(out, tenantResponse(&list[i]))
	}
	c.JSON(http.StatusOK, gin.H{"tenants": out})
}

type updateTenantBody struct {
	Name             *string `json:"name"`
	RoomNumber       *string `json:"room_number"`
	RentAmount       *int    `json:"rent_amount"`
	DueDay           *int16  `json:"due_day"`
	NoticePeriodDays *int16  `json:"notice_period_days"`
}

// UpdateTenant handles PATCH /owner/tenants/:id.
func (h *Handlers) UpdateTenant(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	t, err := h.TenantStore.GetByID(c.Request.Context(), id)
	if err != nil || t.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	var body updateTenantBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if body.Name != nil {
		t.Name = *body.Name
	}
	if body.RoomNumber != nil {
		t.RoomNumber = body.RoomNumber
	}
	if body.RentAmount != nil {
		if *body.RentAmount <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "rent_amount must be positive"})
			return
		}
		t.RentAmount = *body.RentAmount
	}
	if body.DueDay != nil {
		if *body.DueDay < 1 || *body.DueDay > 28 {
			apierr.RespondClientErr(c, http.StatusBadRequest, "due_day must be 1–28", apierr.CodeRequestDueDayInvalid)
			return
		}
		t.DueDay = body.DueDay
	}
	if body.NoticePeriodDays != nil {
		t.NoticePeriodDays = *body.NoticePeriodDays
	}
	if err := h.Tenants.UpdateTenant(c.Request.Context(), t); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, tenantResponse(t))
}

// TenantNotice handles POST /owner/tenants/:id/notice.
func (h *Handlers) TenantNotice(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	t, err := h.TenantStore.GetByID(c.Request.Context(), id)
	if err != nil || t.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	at := time.Now().UTC()
	var body struct {
		NoticeGivenAt *time.Time `json:"notice_given_at"`
	}
	_ = c.ShouldBindJSON(&body)
	if body.NoticeGivenAt != nil {
		at = *body.NoticeGivenAt
	}
	if err := h.Tenants.LogNotice(c.Request.Context(), id, at); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// TenantVacate handles POST /owner/tenants/:id/vacate.
func (h *Handlers) TenantVacate(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	t, err := h.TenantStore.GetByID(c.Request.Context(), id)
	if err != nil || t.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	if err := h.Tenants.Vacate(c.Request.Context(), id); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// TenantAttachPhone handles POST /owner/tenants/:id/attach-phone.
func (h *Handlers) TenantAttachPhone(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	t, err := h.TenantStore.GetByID(c.Request.Context(), id)
	if err != nil || t.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	var body struct {
		Phone string `json:"phone" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone required"})
		return
	}
	if err := h.Tenants.AttachPhone(c.Request.Context(), id, body.Phone); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// TenantProrate handles POST /owner/tenants/:id/prorate.
func (h *Handlers) TenantProrate(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	t, err := h.TenantStore.GetByID(c.Request.Context(), id)
	if err != nil || t.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	var body struct {
		VacateDate time.Time `json:"vacate_date" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "vacate_date required"})
		return
	}
	due, err := h.Billing.Prorate(c.Request.Context(), id, body.VacateDate)
	if err != nil {
		respondErr(c, typedClientErr(http.StatusBadRequest, err, billing.ErrNoOpenRentDue, billing.ErrDueNotWaivable, billing.ErrOpenDueExists))
		return
	}
	c.JSON(http.StatusOK, due)
}

// TenantDepositSettle handles POST /owner/tenants/:id/deposit/settle.
func (h *Handlers) TenantDepositSettle(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	t, err := h.TenantStore.GetByID(c.Request.Context(), id)
	if err != nil || t.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	var body struct {
		RefundedPaise int64  `json:"refunded_amount_paise" binding:"required"`
		Reason        string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "refunded_amount_paise required"})
		return
	}
	if err := h.Payments.SettleDeposit(c.Request.Context(), id, body.RefundedPaise, body.Reason); err != nil {
		respondErr(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func tenantResponse(t *domain.Tenant) gin.H {
	resp := gin.H{
		"id":                   t.ID,
		"property_id":          t.PropertyID,
		"name":                 t.Name,
		"phone":                t.Phone,
		"room_number":          t.RoomNumber,
		"aadhaar_last4":        t.AadhaarLast4,
		"rent_amount":          t.RentAmount,
		"due_day":              t.DueDay,
		"notice_period_days":   t.NoticePeriodDays,
		"notice_given_at":      t.NoticeGivenAt,
		"credit_balance_paise": t.CreditBalancePaise,
		"status":               t.Status,
		"contact_mode":         t.ContactMode(),
		"permanent_address":    t.PermanentAddress,
		"current_address":      t.CurrentAddress,
		"parent_name":          t.ParentName,
		"emergency_phone":      t.EmergencyPhone,
		"joined_on":            t.JoinedOn,
		"has_id_photo":         t.HasIDPhoto,
		"created_at":           t.CreatedAt,
		"updated_at":           t.UpdatedAt,
	}
	return resp
}

// TenantIDPhoto handles GET /owner/tenants/:id/id-photo.
func (h *Handlers) TenantIDPhoto(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	t, err := h.TenantStore.GetByID(c.Request.Context(), id)
	if err != nil || t.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	b, err := h.TenantStore.GetIDPhoto(c.Request.Context(), id)
	if err != nil || len(b) == 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": "no id photo"})
		return
	}
	ct := http.DetectContentType(b)
	c.Data(http.StatusOK, ct, b)
}

// ListDues handles GET /owner/dues.
func (h *Handlers) ListDues(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	f := postgres.DueListFilter{PropertyID: pid}
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
	c.JSON(http.StatusOK, gin.H{"dues": list})
}

// WaiveDue handles POST /owner/dues/:id/waive.
func (h *Handlers) WaiveDue(c *gin.Context) {
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
		AmountPaise int    `json:"amount" binding:"required"`
		TxnID       string `json:"upi_txn_id" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "amount and upi_txn_id required"})
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
		AmountPaise int    `json:"amount" binding:"required"`
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
		rupees := float64(due.Amount) / 100.0
		msg := "Pay rent ₹" + strconv.FormatFloat(rupees, 'f', 0, 64) + " — " + url
		wa = magiclink.BuildWALink(tenant.Phone, msg)
	}
	c.JSON(http.StatusOK, gin.H{"path": path, "url": url, "wa_me": wa})
}

const maxCSVBytes = 5 << 20

// ImportStatements handles POST /owner/statements/import.
func (h *Handlers) ImportStatements(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	file, hdr, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file required"})
		return
	}
	defer file.Close()
	if hdr.Size > maxCSVBytes {
		c.JSON(http.StatusBadRequest, gin.H{"error": "file too large (max 5MB)"})
		return
	}
	limited := io.LimitReader(file, maxCSVBytes+1)
	rows, err := csv.Parse(limited)
	if err != nil {
		if errors.Is(err, csv.ErrUnknownSchema) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown CSV schema — required columns: txn_id, amount, date, note"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "csv parse error: unsupported format"})
		return
	}

	matched, failed := 0, 0
	for _, row := range rows {
		if _, err := h.Payments.MatchPayment(c.Request.Context(), pid, row.TxnID, row.AmountPaise, row.Date, row.Note); err != nil {
			failed++
			if h.Finance != nil && h.FinanceEnabled {
				_ = h.Finance.SuggestCSVDebit(c.Request.Context(), pid, row.TxnID, row.AmountPaise, row.Date, row.Note)
			}
			continue
		}
		matched++
	}
	if matched > 0 {
		if err := h.ImportStore.Create(c.Request.Context(), &postgres.ImportLog{
			PropertyID: pid,
			Filename:   hdr.Filename,
			RowCount:   len(rows),
			ImportedBy: uid,
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "import log failed", "matched": matched, "failed": failed})
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"row_count": len(rows),
		"matched":   matched,
		"failed":    failed,
	})
}

// ListPayments handles GET /owner/payments.
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
	list, err := h.PaymentStore.ListByProperty(c.Request.Context(), pid, mb)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"payments": list})
}

// ListEvents handles GET /owner/events.
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
	if lim := c.Query("limit"); lim != "" {
		if n, err := strconv.Atoi(lim); err == nil {
			f.Limit = n
		}
	}
	list, err := h.EventStore.List(c.Request.Context(), f)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	// Hide internal bigserial ids in JSON via domain.Event `json:"-"` on ID.
	c.JSON(http.StatusOK, gin.H{"events": list})
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
