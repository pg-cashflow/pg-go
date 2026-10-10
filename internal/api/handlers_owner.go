package api

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/billing"
	"github.com/pg-cashflow/pg-go/internal/collector"
	"github.com/pg-cashflow/pg-go/internal/csv"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/magiclink"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/qr"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// ListProperties handles GET /owner/properties — returns all properties owned by the authenticated owner.
func (h *Handlers) ListProperties(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		apierr.RespondClientErr(c, http.StatusUnauthorized, "unauthorized", apierr.CodeAuthUnauthorized)
		return
	}
	var phone, email string
	if h.UserStore != nil {
		if u, err := h.UserStore.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
			phone = u.Phone
			email = u.Email
		}
	} else if h.AuthUserRepo != nil {
		if u, err := h.AuthUserRepo.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
			phone = u.Phone
			email = u.Email
		}
	}
	all, err := h.PropertyStore.List(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	out := make([]domain.Property, 0, len(all))
	for _, p := range all {
		isMatch := false
		if claims.PropertyID != nil && p.ID == *claims.PropertyID {
			isMatch = true
		} else if phone != "" && p.OwnerPhone == phone {
			isMatch = true
		} else if email != "" && strings.EqualFold(p.OwnerEmail, email) {
			isMatch = true
		}
		if isMatch {
			p.UPIVPA = "" // never expose in API
			out = append(out, p)
		}
	}
	c.JSON(http.StatusOK, gin.H{"properties": out})
}

type createPropertyBody struct {
	Name        string `json:"name" binding:"required"`
	Address     string `json:"address"`
	OwnerName   string `json:"owner_name"`
	PaymentMode string `json:"payment_mode"`
}

// CreateProperty handles POST /owner/properties — creates an additional property for the authenticated owner.
func (h *Handlers) CreateProperty(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims.Role != domain.RoleOwner {
		apierr.RespondClientErr(c, http.StatusForbidden, "forbidden", apierr.CodeAuthForbidden)
		return
	}
	var body createPropertyBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "invalid property body", apierr.CodeRequestInvalidBody)
		return
	}

	var userPhone, userEmail string
	if h.UserStore != nil {
		if u, err := h.UserStore.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
			userPhone = u.Phone
			userEmail = u.Email
		}
	} else if h.AuthUserRepo != nil {
		if u, err := h.AuthUserRepo.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
			userPhone = u.Phone
			userEmail = u.Email
		}
	}

	prop := domain.Property{
		Name:        strings.TrimSpace(body.Name),
		OwnerPhone:  userPhone,
		OwnerEmail:  userEmail,
		OwnerName:   strings.TrimSpace(body.OwnerName),
		PaymentMode: body.PaymentMode,
	}
	if body.Address != "" {
		trimmed := strings.TrimSpace(body.Address)
		prop.Address = &trimmed
	}
	if err := h.PropertyStore.Create(c.Request.Context(), &prop); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "create property failed"})
		return
	}
	prop.UPIVPA = ""
	c.JSON(http.StatusCreated, gin.H{"property": prop})
}

// OwnerSwitchProperty handles POST /owner/properties/:id/switch — switches active property for single login.
func (h *Handlers) OwnerSwitchProperty(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok || claims.Role != domain.RoleOwner {
		apierr.RespondClientErr(c, http.StatusForbidden, "forbidden", apierr.CodeAuthForbidden)
		return
	}
	propIDStr := c.Param("id")
	targetID, err := uuid.Parse(propIDStr)
	if err != nil || targetID == uuid.Nil {
		apierr.RespondClientErr(c, http.StatusBadRequest, "invalid property id", apierr.CodeRequestInvalidId)
		return
	}

	prop, err := h.PropertyStore.GetByID(c.Request.Context(), targetID)
	if err != nil || prop == nil {
		apierr.RespondClientErr(c, http.StatusForbidden, "property not found or not owned", apierr.CodeAuthForbidden)
		return
	}

	var userPhone, userEmail string
	var user *domain.User
	if h.UserStore != nil {
		user, _ = h.UserStore.GetByID(c.Request.Context(), claims.UserID)
	} else if h.AuthUserRepo != nil {
		user, _ = h.AuthUserRepo.GetByID(c.Request.Context(), claims.UserID)
	}
	if user != nil {
		userPhone = user.Phone
		userEmail = user.Email
	}

	authorized := false
	if claims.PropertyID != nil && *claims.PropertyID == targetID {
		authorized = true
	} else if userPhone != "" && prop.OwnerPhone == userPhone {
		authorized = true
	} else if userEmail != "" && strings.EqualFold(prop.OwnerEmail, userEmail) {
		authorized = true
	}

	if !authorized {
		apierr.RespondClientErr(c, http.StatusForbidden, "forbidden: property not owned by user", apierr.CodeAuthForbidden)
		return
	}

	if h.UserStore != nil {
		_ = h.UserStore.SetPropertyID(c.Request.Context(), claims.UserID, targetID)
	}

	prop.UPIVPA = ""
	c.JSON(http.StatusOK, gin.H{"ok": true, "property": prop})
}

// ResolveOwnerPropertyScope inspects X-Property-ID header or property_id query param
// and binds the verified property ID into requestscope.
func (h *Handlers) ResolveOwnerPropertyScope() gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := auth.ClaimsFromContext(c)
		if !ok || claims.Role != domain.RoleOwner {
			c.Next()
			return
		}

		targetStr := c.GetHeader("X-Property-ID")
		if targetStr == "" {
			targetStr = c.Query("property_id")
		}

		if targetStr == "" {
			if claims.PropertyID != nil && *claims.PropertyID != uuid.Nil {
				c.Request = c.Request.WithContext(requestscope.WithPropertyID(c.Request.Context(), *claims.PropertyID))
			}
			c.Next()
			return
		}

		targetID, err := uuid.Parse(strings.TrimSpace(targetStr))
		if err != nil || targetID == uuid.Nil {
			apierr.RespondClientErr(c, http.StatusBadRequest, "invalid property id", apierr.CodeRequestInvalidId)
			c.Abort()
			return
		}

		if claims.PropertyID != nil && *claims.PropertyID == targetID {
			c.Request = c.Request.WithContext(requestscope.WithPropertyID(c.Request.Context(), targetID))
			c.Next()
			return
		}

		// Fail closed: a cross-property switch must be verified against the property
		// store. Without a store we cannot prove ownership, so the request is refused
		// rather than binding an arbitrary property ID into the request scope.
		if h.PropertyStore == nil {
			apierr.RespondClientErr(c, http.StatusForbidden, "forbidden: property not owned by user", apierr.CodeAuthForbidden)
			c.Abort()
			return
		}
		prop, err := h.PropertyStore.GetByID(c.Request.Context(), targetID)
		if err != nil || prop == nil {
			apierr.RespondClientErr(c, http.StatusForbidden, "property not found or not owned", apierr.CodeAuthForbidden)
			c.Abort()
			return
		}

		var userPhone, userEmail string
		if h.UserStore != nil {
			if u, err := h.UserStore.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
				userPhone = u.Phone
				userEmail = u.Email
			}
		} else if h.AuthUserRepo != nil {
			if u, err := h.AuthUserRepo.GetByID(c.Request.Context(), claims.UserID); err == nil && u != nil {
				userPhone = u.Phone
				userEmail = u.Email
			}
		}

		authorized := false
		if userPhone != "" && prop.OwnerPhone == userPhone {
			authorized = true
		} else if userEmail != "" && strings.EqualFold(prop.OwnerEmail, userEmail) {
			authorized = true
		}

		if !authorized {
			apierr.RespondClientErr(c, http.StatusForbidden, "forbidden: property not owned by user", apierr.CodeAuthForbidden)
			c.Abort()
			return
		}

		c.Request = c.Request.WithContext(requestscope.WithPropertyID(c.Request.Context(), targetID))
		c.Next()
	}
}

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
		RefundedPaise int64  `json:"refunded_amount_paise" binding:"required"`
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
	if err := h.Payments.SettleDeposit(c.Request.Context(), id, body.RefundedPaise, body.Reason); err != nil {
		respondErr(c, paymentClientErr(err))
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
		// Minor Protection & Guardian KYC (ADR migration 021, CONTRACT Rev 13 §1)
		"majority_date":                 t.MajorityDate,
		"guardian_name":                 t.GuardianName,
		"guardian_phone":                t.GuardianPhone,
		"guardian_relation":             t.GuardianRelation,
		"guardian_kyc_reference_id":     t.GuardianKYCReferenceID,
		"guardian_consent_verified_at":  t.GuardianConsentVerifiedAt,
		"is_gamification_disabled":      t.IsGamificationDisabled,
		"is_minor":                      t.IsMinor(time.Now().UTC()),
		"created_at":                    t.CreatedAt,
		"updated_at":                    t.UpdatedAt,
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
	offset := 0
	if off := c.Query("offset"); off != "" {
		if n, err := strconv.Atoi(off); err == nil && n > 0 {
			offset = n
		}
	}
	f := postgres.DueListFilter{PropertyID: pid, Limit: limit, Offset: offset}
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
	resp := gin.H{"dues": list, "limit": limit, "offset": offset}
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
		respondErr(c, clientErr(http.StatusBadRequest, "file required"))
		return
	}
	defer file.Close()
	if hdr.Size > maxCSVBytes {
		respondErr(c, clientErr(http.StatusBadRequest, "file too large (max 5MB)"))
		return
	}
	limited := io.LimitReader(file, maxCSVBytes+1)
	parseRes, err := csv.ParseWithMeta(limited)
	if err != nil {
		if errors.Is(err, csv.ErrUnknownSchema) {
			respondErr(c, clientErr(http.StatusBadRequest, "unknown CSV schema — required columns: date, note, and amount (or deposit/withdrawal)"))
			return
		}
		respondErr(c, clientErr(http.StatusBadRequest, "csv parse error: invalid format or syntax"))
		return
	}
	rows := parseRes.Rows

	var bankAccountID *uuid.UUID
	if acctStr := strings.TrimSpace(c.Request.FormValue("bank_account_id")); acctStr != "" {
		if parsedID, err := uuid.Parse(acctStr); err == nil {
			bankAccountID = &parsedID
		}
	}

	var (
		matched    int
		suggested  int
		unmatched  int
		duplicates int
		debits     int
	)

	for _, row := range rows {
		// 1. Debits: Outflows stored for statement reconciliation tie-out, but never matched to dues or credited to ledger
		if row.Type == csv.RowTypeDebit {
			classification := "unclassified"
			if row.IsInternalTransfer {
				classification = "internal_transfer"
			}
			debitHash := domain.ComputeBankTxnDedupHash(
				pid,
				bankAccountID,
				row.Date,
				row.AmountPaise,
				"debit",
				row.TxnID,
				row.BalancePaise,
				row.Occurrence,
			)
			debitTxn := &domain.BankTransaction{
				ID:                  uuid.NewSHA1(uuid.NameSpaceOID, []byte(debitHash)),
				PropertyID:          pid,
				BankAccountID:       bankAccountID,
				TxnID:               row.TxnID,
				AmountPaise:         row.AmountPaise,
				RowType:             "debit",
				TxnDate:             row.Date,
				Narration:           row.Note,
				ClosingBalancePaise: row.BalancePaise,
				OccurrenceIndex:     row.Occurrence,
				DedupHash:           debitHash,
				Status:              domain.BankTxnIgnoredDebit,
				Classification:      classification,
				IsInternalTransfer:  row.IsInternalTransfer,
				PayerPhone:          row.PayerPhone,
				PayerVPA:            row.PayerVPA,
			}
			if h.BankTxnRepo != nil {
				inserted, err := h.BankTxnRepo.InsertTransaction(c.Request.Context(), nil, debitTxn)
				if err != nil {
					slog.Error("ImportStatements saving debit transaction failed", "error", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record debit transaction"})
					return
				}
				if !inserted {
					duplicates++
					continue
				}
			}
			debits++
			continue
		}

		// 2. Credits: Inflow handling with idempotent deduplication and double-entry quarantine
		classification := "unclassified"
		if row.IsReversal {
			classification = "reversal"
		} else if row.IsInternalTransfer {
			classification = "internal_transfer"
		}

		dedupHash := domain.ComputeBankTxnDedupHash(
			pid,
			bankAccountID,
			row.Date,
			row.AmountPaise,
			"credit",
			row.TxnID,
			row.BalancePaise,
			row.Occurrence,
		)
		sourceID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(dedupHash))

		creditTxn := &domain.BankTransaction{
			ID:                  sourceID,
			PropertyID:          pid,
			BankAccountID:       bankAccountID,
			TxnID:               row.TxnID,
			AmountPaise:         row.AmountPaise,
			RowType:             "credit",
			TxnDate:             row.Date,
			Narration:           row.Note,
			ClosingBalancePaise: row.BalancePaise,
			OccurrenceIndex:     row.Occurrence,
			DedupHash:           dedupHash,
			Status:              domain.BankTxnUnmatched,
			Classification:      classification,
			IsReversal:          row.IsReversal,
			IsInternalTransfer:  row.IsInternalTransfer,
			PayerPhone:          row.PayerPhone,
			PayerVPA:            row.PayerVPA,
		}

		// Fail-fast double-entry quarantine posted FIRST: Dr bank / Cr unapplied_receipts.
		// Posting the journal entry with deterministic sourceID first guarantees that if this step fails,
		// the bank_transaction row has not committed, allowing safe retry. On retry, the journal's
		// unique constraint (source_type, source_id, line_kind) makes the entry idempotent.
		if h.Finance != nil && h.FinanceEnabled {
			if err := h.Finance.MirrorBankStatementCredit(c.Request.Context(), pid, sourceID, row.AmountPaise, row.Date); err != nil {
				slog.Error("ImportStatements ledger mirror quarantine failed", "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record financial entry"})
				return
			}
		}

		if h.BankTxnRepo != nil {
			inserted, err := h.BankTxnRepo.InsertTransaction(c.Request.Context(), nil, creditTxn)
			if err != nil {
				slog.Error("ImportStatements inserting bank transaction failed", "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record bank transaction"})
				return
			}
			if !inserted {
				// Explicit duplicate skipping
				duplicates++
				continue
			}
		}

		// Reversals and internal transfers are strictly excluded from dues matching
		if row.IsReversal || row.IsInternalTransfer {
			unmatched++
			continue
		}

		// Evaluate match candidates without auto-settling (Tier 1 auto-posting is held OFF until verified against live statement)
		matchRes, err := h.Payments.SuggestMatch(c.Request.Context(), pid, row.AmountPaise, row.Date, row.Note)
		if err == nil && matchRes != nil && matchRes.Due != nil {
			suggested++
			creditTxn.Status = domain.BankTxnSuggestedMatch
			creditTxn.SuggestedDueID = &matchRes.Due.ID
			if matchRes.IsDeterministic {
				creditTxn.ConfidenceScore = 1.00
			} else {
				creditTxn.ConfidenceScore = 0.80
			}
			if h.BankTxnRepo != nil {
				if err := h.BankTxnRepo.UpdateStatus(c.Request.Context(), nil, creditTxn.ID, creditTxn.Status, nil, nil, nil, nil); err != nil {
					slog.Error("ImportStatements updating transaction status failed", "error", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update transaction status"})
					return
				}
			}
		} else {
			unmatched++
		}
	}

	if (matched + suggested + unmatched + debits) > 0 {
		if h.ImportStore != nil {
			if err := h.ImportStore.Create(c.Request.Context(), &postgres.ImportLog{
				PropertyID: pid,
				Filename:   hdr.Filename,
				RowCount:   len(rows),
				ImportedBy: uid,
			}); err != nil {
				slog.Error("ImportStatements recording import log failed", "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record import log"})
				return
			}
		}
	}

	resp := gin.H{
		"row_count":  len(rows),
		"matched":    matched,   // 0 while Tier 1 auto-posting is held off for verification
		"suggested":  suggested, // candidate matches staged for owner confirmation
		"unmatched":  unmatched, // credits requiring manual allocation or refund
		"duplicates": duplicates,
		"debits":     debits,    // tagged outflows stored for statement reconciliation tie-out
		"failed":     unmatched, // backward-compatibility alias
	}
	if parseRes.OpeningBalancePaise != nil {
		resp["opening_balance_paise"] = *parseRes.OpeningBalancePaise
	}
	if len(parseRes.Warnings) > 0 {
		resp["warnings"] = parseRes.Warnings
	}
	c.JSON(http.StatusOK, resp)
}

type ClassifyBankTransactionReq struct {
	Classification string `json:"classification" binding:"required"`
}

// ClassifyBankTransaction handles POST /owner/statements/transactions/:id/classify.
func (h *Handlers) ClassifyBankTransaction(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	txnID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req ClassifyBankTransactionReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: classification is required"})
		return
	}

	validClassifications := map[string]string{
		"interest_income":   domain.AcctInterestIncome,
		"owner_equity":      domain.AcctOwnerCapital,
		"non_pg_income":     domain.AcctNonPGOtherIncome,
		"reversal":          domain.AcctOperatingExpense,
		"internal_transfer": domain.AcctBank,
	}
	targetAcct, valid := validClassifications[req.Classification]
	if !valid {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid classification; must be interest_income, owner_equity, non_pg_income, reversal, or internal_transfer"})
		return
	}

	if h.BankTxnRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "bank transaction store not configured"})
		return
	}

	txn, err := h.BankTxnRepo.GetByPropertyAndID(c.Request.Context(), pid, txnID)
	if err != nil {
		if errors.Is(err, postgres.ErrBankTransactionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "bank transaction not found"})
			return
		}
		slog.Error("ClassifyBankTransaction retrieving txn failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve transaction"})
		return
	}

	var entryID *uuid.UUID
	if h.Finance != nil && h.FinanceEnabled && txn.RowType == "credit" {
		if err := h.Finance.MirrorUnappliedReclassification(c.Request.Context(), pid, txn.ID, targetAcct, txn.AmountPaise, txn.TxnDate); err != nil {
			slog.Error("ClassifyBankTransaction ledger mirror reclassification failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record financial reclassification"})
			return
		}
	}

	if err := h.BankTxnRepo.Reclassify(c.Request.Context(), nil, txn.ID, req.Classification, entryID); err != nil {
		slog.Error("ClassifyBankTransaction reclassify failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update classification"})
		return
	}

	txn.Classification = req.Classification
	c.JSON(http.StatusOK, txn)
}

// ListBankTransactions handles GET /owner/statements/transactions.
// Query filters: status, from_date, to_date, limit, offset.
func (h *Handlers) ListBankTransactions(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if h.BankTxnRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "bank transaction store not configured"})
		return
	}

	filter := domain.BankTransactionFilter{}
	if st := strings.TrimSpace(c.Query("status")); st != "" {
		status := domain.BankTransactionStatus(st)
		switch status {
		case domain.BankTxnMatched, domain.BankTxnSuggestedMatch, domain.BankTxnUnmatched, domain.BankTxnRefunded, domain.BankTxnIgnoredDebit:
			filter.Status = &status
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status filter"})
			return
		}
	}
	if fd := strings.TrimSpace(c.Query("from_date")); fd != "" {
		if t, err := time.Parse("2006-01-02", fd); err == nil {
			filter.FromDate = &t
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from_date format; use YYYY-MM-DD"})
			return
		}
	}
	if td := strings.TrimSpace(c.Query("to_date")); td != "" {
		if t, err := time.Parse("2006-01-02", td); err == nil {
			filter.ToDate = &t
		} else {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid to_date format; use YYYY-MM-DD"})
			return
		}
	}
	if lim := strings.TrimSpace(c.Query("limit")); lim != "" {
		if l, err := strconv.Atoi(lim); err == nil && l > 0 {
			filter.Limit = l
		}
	}
	if off := strings.TrimSpace(c.Query("offset")); off != "" {
		if o, err := strconv.Atoi(off); err == nil && o >= 0 {
			filter.Offset = o
		}
	}

	txns, total, err := h.BankTxnRepo.ListByProperty(c.Request.Context(), pid, filter)
	if err != nil {
		slog.Error("ListBankTransactions failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list bank transactions"})
		return
	}
	if txns == nil {
		txns = []*domain.BankTransaction{}
	}

	c.JSON(http.StatusOK, gin.H{
		"transactions": txns,
		"total":        total,
		"limit":        filter.Limit,
		"offset":       filter.Offset,
	})
}

type ConfirmBankTransactionReq struct {
	DueID *uuid.UUID `json:"due_id"`
}

// ConfirmBankTransactionMatch handles POST /owner/statements/transactions/:id/confirm.
func (h *Handlers) ConfirmBankTransactionMatch(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	txnID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	if h.BankTxnRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "bank transaction store not configured"})
		return
	}

	txn, err := h.BankTxnRepo.GetByPropertyAndID(c.Request.Context(), pid, txnID)
	if err != nil {
		if errors.Is(err, postgres.ErrBankTransactionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "bank transaction not found"})
			return
		}
		slog.Error("ConfirmBankTransactionMatch retrieving txn failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve bank transaction"})
		return
	}

	if txn.Status == domain.BankTxnMatched {
		c.JSON(http.StatusOK, gin.H{"transaction": txn, "status": "already_matched"})
		return
	}
	if txn.Status == domain.BankTxnRefunded {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot match refunded bank transaction"})
		return
	}
	if txn.RowType != "credit" || txn.AmountPaise <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only positive credit transactions can be matched to dues"})
		return
	}

	var req ConfirmBankTransactionReq
	if c.Request.ContentLength > 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
	}

	targetDueID := req.DueID
	if targetDueID == nil || *targetDueID == uuid.Nil {
		targetDueID = txn.SuggestedDueID
	}
	if targetDueID == nil || *targetDueID == uuid.Nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "due_id is required"})
		return
	}

	if h.DueStore == nil || h.Payments == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "payment service or due store not configured"})
		return
	}

	due, err := h.DueStore.GetByID(c.Request.Context(), *targetDueID)
	if err != nil || due == nil || due.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "due not found"})
		return
	}
	if due.Status == domain.DueStatusPaid || due.Status == domain.DueStatusWaived {
		c.JSON(http.StatusBadRequest, gin.H{"error": "due is already paid or waived"})
		return
	}

	pay, err := h.Payments.ManualMatch(c.Request.Context(), due.ID, txn.AmountPaise, txn.TxnID, uid)
	if err != nil {
		respondErr(c, paymentClientErr(err))
		return
	}

	if h.Finance != nil && h.FinanceEnabled {
		if err := h.Finance.MirrorUnappliedAllocation(c.Request.Context(), pid, txn.ID, due.Kind, txn.AmountPaise, txn.TxnDate); err != nil {
			slog.Error("ConfirmBankTransactionMatch ledger mirror failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record financial allocation"})
			return
		}
	}

	now := time.Now()
	if err := h.BankTxnRepo.UpdateStatus(c.Request.Context(), nil, txn.ID, domain.BankTxnMatched, &due.ID, &uid, &now, nil); err != nil {
		slog.Error("ConfirmBankTransactionMatch updating txn status failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update bank transaction status"})
		return
	}

	txn.Status = domain.BankTxnMatched
	txn.MatchedDueID = &due.ID
	txn.MatchedBy = &uid
	txn.MatchedAt = &now

	c.JSON(http.StatusOK, gin.H{
		"transaction": txn,
		"payment":     pay,
	})
}

// RefundBankTransaction handles POST /owner/statements/transactions/:id/refund.
func (h *Handlers) RefundBankTransaction(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	txnID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	if h.BankTxnRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "bank transaction store not configured"})
		return
	}

	txn, err := h.BankTxnRepo.GetByPropertyAndID(c.Request.Context(), pid, txnID)
	if err != nil {
		if errors.Is(err, postgres.ErrBankTransactionNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "bank transaction not found"})
			return
		}
		slog.Error("RefundBankTransaction retrieving txn failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve bank transaction"})
		return
	}

	if txn.Status == domain.BankTxnRefunded {
		c.JSON(http.StatusOK, gin.H{"transaction": txn, "status": "already_refunded"})
		return
	}
	if txn.Status == domain.BankTxnMatched {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot refund matched bank transaction; reverse payment first"})
		return
	}
	if txn.RowType != "credit" || txn.AmountPaise <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only positive credit deposits can be refunded"})
		return
	}

	if h.Finance != nil && h.FinanceEnabled {
		if err := h.Finance.MirrorBankDepositRefund(c.Request.Context(), pid, txn.ID, txn.AmountPaise, txn.TxnDate); err != nil {
			slog.Error("RefundBankTransaction ledger mirror failed", "error", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record financial refund"})
			return
		}
	}

	now := time.Now()
	if err := h.BankTxnRepo.UpdateStatus(c.Request.Context(), nil, txn.ID, domain.BankTxnRefunded, nil, &uid, &now, nil); err != nil {
		slog.Error("RefundBankTransaction updating txn status failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update bank transaction status"})
		return
	}

	txn.Status = domain.BankTxnRefunded
	txn.MatchedBy = &uid
	txn.MatchedAt = &now

	c.JSON(http.StatusOK, gin.H{"transaction": txn})
}

// ListBankAccounts handles GET /owner/bank-accounts.
func (h *Handlers) ListBankAccounts(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if h.BankAccountRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "bank account store not configured"})
		return
	}

	accts, err := h.BankAccountRepo.ListByProperty(c.Request.Context(), pid)
	if err != nil {
		slog.Error("ListBankAccounts failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list bank accounts"})
		return
	}
	if accts == nil {
		accts = []*domain.BankAccount{}
	}

	c.JSON(http.StatusOK, gin.H{"bank_accounts": accts})
}

type CreateBankAccountReq struct {
	BankName           string                 `json:"bank_name" binding:"required"`
	AccountType        domain.BankAccountType `json:"account_type"`
	AccountNumberLast4 string                 `json:"account_number_last4" binding:"required"`
	Label              string                 `json:"label"`
	StatementProfile   string                 `json:"statement_profile"`
}

// CreateBankAccount handles POST /owner/bank-accounts.
func (h *Handlers) CreateBankAccount(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if h.BankAccountRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "bank account store not configured"})
		return
	}

	var req CreateBankAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: bank_name and account_number_last4 are required"})
		return
	}

	req.BankName = strings.TrimSpace(req.BankName)
	if req.BankName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bank_name cannot be empty"})
		return
	}

	req.AccountNumberLast4 = strings.TrimSpace(req.AccountNumberLast4)
	if len(req.AccountNumberLast4) != 4 || !isAllDigits(req.AccountNumberLast4) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_number_last4 must be exactly 4 digits"})
		return
	}

	if req.AccountType == "" {
		req.AccountType = domain.BankAccountTypeSavings
	}
	if req.AccountType != domain.BankAccountTypeSavings && req.AccountType != domain.BankAccountTypeCurrent {
		c.JSON(http.StatusBadRequest, gin.H{"error": "account_type must be either 'savings' or 'current'"})
		return
	}

	if req.StatementProfile == "" {
		req.StatementProfile = "generic"
	}

	acct := &domain.BankAccount{
		ID:                 uuid.New(),
		PropertyID:         pid,
		BankName:           req.BankName,
		AccountType:        req.AccountType,
		AccountNumberLast4: req.AccountNumberLast4,
		Label:              strings.TrimSpace(req.Label),
		StatementProfile:   strings.TrimSpace(req.StatementProfile),
		IsActive:           true,
	}

	if err := h.BankAccountRepo.Create(c.Request.Context(), acct); err != nil {
		slog.Error("CreateBankAccount failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create bank account"})
		return
	}

	c.JSON(http.StatusCreated, acct)
}

// DeactivateBankAccount handles DELETE /owner/bank-accounts/:id.
func (h *Handlers) DeactivateBankAccount(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	acctID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	if h.BankAccountRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "bank account store not configured"})
		return
	}

	acct, err := h.BankAccountRepo.GetByID(c.Request.Context(), acctID)
	if err != nil {
		if errors.Is(err, postgres.ErrBankAccountNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "bank account not found"})
			return
		}
		slog.Error("DeactivateBankAccount retrieving acct failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retrieve bank account"})
		return
	}

	if acct.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "bank account not found"})
		return
	}

	if err := h.BankAccountRepo.Deactivate(c.Request.Context(), acctID); err != nil {
		slog.Error("DeactivateBankAccount failed", "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to deactivate bank account"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"status": "deactivated"})
}

func isAllDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ListPayments handles GET /owner/payments (Requirement 11 pagination, max 50).
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
	if cur := c.Query("cursor"); cur != "" {
		if t, err := time.Parse(time.RFC3339, cur); err == nil {
			before = &t
		}
	}
	if paginated, ok := h.PaymentStore.(interface {
		ListByPropertyPaginated(ctx context.Context, propertyID uuid.UUID, matchedBy *domain.MatchedBy, limit int, before *time.Time) ([]domain.Payment, error)
	}); ok {
		list, err := paginated.ListByPropertyPaginated(c.Request.Context(), pid, mb, limit, before)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
			return
		}
		var nextCursor *string
		if len(list) == limit {
			lastTime := list[len(list)-1].MatchedAt.Format(time.RFC3339)
			nextCursor = &lastTime
		}
		c.JSON(http.StatusOK, gin.H{"payments": list, "limit": limit, "next_cursor": nextCursor})
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
