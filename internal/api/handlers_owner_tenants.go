package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/billing"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type createTenantBody struct {
	Name             string  `json:"name" binding:"required"`
	Phone            *string `json:"phone"`
	RoomNumber       *string `json:"room_number"`
	RentAmount       int64   `json:"rent_amount" binding:"required"`
	DueDay           int16   `json:"due_day" binding:"required"`
	NoticePeriodDays int16   `json:"notice_period_days"`
	DepositAmount    *int64  `json:"deposit_amount"`
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
	RentAmount       *int64  `json:"rent_amount"`
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
	auth.InvalidateTenantCache(id)
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
	auth.InvalidateTenantCache(id)
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
	uid, ok := userIDFromClaims(c)
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
		RefundedPaise *int64 `json:"refunded_amount_paise" binding:"required"`
		Reason        string `json:"reason"`
		StepUpAuthInput
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "refunded_amount_paise required"})
		return
	}
	if _, ok := h.verifyDualControlOrStepUp(c, pid, uid, nil, body.StepUpAuthInput); !ok {
		return
	}
	if err := h.Payments.SettleDeposit(c.Request.Context(), id, *body.RefundedPaise, body.Reason); err != nil {
		respondErr(c, paymentClientErr(err))
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func maskOptionalPhone(p *string) *string {
	if p == nil {
		return nil
	}
	s := strings.TrimSpace(*p)
	if len(s) <= 4 {
		m := "****"
		return &m
	}
	m := strings.Repeat("*", len(s)-4) + s[len(s)-4:]
	return &m
}

func maskSensitiveRef(r *string) *string {
	if r == nil {
		return nil
	}
	s := strings.TrimSpace(*r)
	if len(s) <= 4 {
		m := "****"
		return &m
	}
	m := "ref_***" + s[len(s)-4:]
	return &m
}

func maskAadhaarLast4(last4 *string) *string {
	if last4 == nil {
		return nil
	}
	s := strings.TrimSpace(*last4)
	if len(s) == 0 {
		return nil
	}
	m := "XXXX-XXXX-" + s
	return &m
}

func tenantResponse(t *domain.Tenant) gin.H {
	resp := gin.H{
		"id":                   t.ID,
		"property_id":          t.PropertyID,
		"name":                 t.Name,
		"phone":                t.Phone,
		"room_number":          t.RoomNumber,
		"aadhaar_last4":        maskAadhaarLast4(t.AadhaarLast4),
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
		// Minor Protection & Guardian KYC (ADR migration 021, CONTRACT Rev 13 §1)
		"majority_date":                t.MajorityDate,
		"guardian_name":                t.GuardianName,
		"guardian_phone":               maskOptionalPhone(t.GuardianPhone),
		"guardian_relation":            t.GuardianRelation,
		"guardian_kyc_reference_id":    maskSensitiveRef(t.GuardianKYCReferenceID),
		"guardian_consent_verified_at": t.GuardianConsentVerifiedAt,
		"is_gamification_disabled":     t.IsGamificationDisabled,
		"is_minor":                     t.IsMinor(time.Now().UTC()),
		"created_at":                   t.CreatedAt,
		"updated_at":                   t.UpdatedAt,
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

// ListDues handles GET /owner/dues (Requirement 11 pagination).
