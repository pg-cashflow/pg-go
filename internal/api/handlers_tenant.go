package api

import (
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/aadhaar"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/localization"
)

// aadhaarLast4Pattern allows exactly four ASCII digits.
var aadhaarLast4Pattern = regexp.MustCompile(`^[0-9]{4}$`)

// TenantPatchMe handles PATCH /tenant/me (limited self-service profile fields).
func (h *Handlers) TenantPatchMe(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	cloned := *t
	updatedTenant := &cloned

	var body struct {
		Name          *string `json:"name"`
		GuardianPhone *string `json:"guardian_phone"`
		Locale        *string `json:"locale"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if body.Name != nil {
		name := strings.TrimSpace(*body.Name)
		if name == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "name cannot be empty"})
			return
		}
		updatedTenant.Name = name
	}
	if body.GuardianPhone != nil {
		phone := strings.TrimSpace(*body.GuardianPhone)
		if phone != "" {
			updatedTenant.GuardianPhone = &phone
		}
	}
	if body.Locale != nil {
		locale := strings.TrimSpace(*body.Locale)
		if locale == "" {
			respondErr(c, clientErrWithCode(http.StatusBadRequest, "locale cannot be empty", apierr.CodePreferencesLocaleRequired))
			return
		}
		if !localization.IsValid(locale) {
			respondErr(c, clientErrWithCode(http.StatusBadRequest, "unsupported locale", apierr.CodePreferencesInvalidLocale))
			return
		}
		if h.PreferencesStore != nil {
			claims, _ := auth.ClaimsFromContext(c)
			if claims != nil {
				if _, err := h.PreferencesStore.Upsert(c.Request.Context(), claims.UserID, &locale, nil); err != nil {
					respondErr(c, err)
					return
				}
			}
		}
	}
	if h.TenantStore != nil {
		if err := h.TenantStore.Update(c.Request.Context(), updatedTenant); err != nil {
			respondErr(c, err)
			return
		}
	}
	*t = *updatedTenant
	auth.InvalidateTenantCache(t.ID)
	c.JSON(http.StatusOK, tenantResponse(updatedTenant))
}

// TenantMe handles GET /tenant/me.
func (h *Handlers) TenantMe(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	dues, err := h.DueStore.ListByTenant(c.Request.Context(), t.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list dues"})
		return
	}
	active := make([]domain.Due, 0)
	for _, d := range dues {
		if d.Status == domain.DueStatusPending || d.Status == domain.DueStatusPartial {
			active = append(active, d)
		}
	}
	resp := tenantResponse(t)
	resp["active_dues"] = active
	c.JSON(http.StatusOK, resp)
}

// TenantDues handles GET /tenant/dues.
func (h *Handlers) TenantDues(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	list, err := h.DueStore.ListByTenant(c.Request.Context(), t.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	ptrList := make([]*domain.Due, len(list))
	var totalOutstanding int64
	for i := range list {
		ptrList[i] = &list[i]
		if list[i].Status == domain.DueStatusPending || list[i].Status == domain.DueStatusPartial {
			totalOutstanding += list[i].Amount
		}
	}
	options := domain.CalculatePaymentOptions(ptrList)
	c.JSON(http.StatusOK, gin.H{
		"dues":                    list,
		"total_outstanding_paise": totalOutstanding,
		"options":                 options,
	})
}

// TenantDuesOptions handles GET /tenant/dues/options.
func (h *Handlers) TenantDuesOptions(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	list, err := h.DueStore.ListByTenant(c.Request.Context(), t.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	ptrList := make([]*domain.Due, len(list))
	var totalOutstanding int64
	for i := range list {
		ptrList[i] = &list[i]
		if list[i].Status == domain.DueStatusPending || list[i].Status == domain.DueStatusPartial {
			totalOutstanding += list[i].Amount
		}
	}
	options := domain.CalculatePaymentOptions(ptrList)
	c.JSON(http.StatusOK, gin.H{
		"total_outstanding_paise": totalOutstanding,
		"options":                 options,
	})
}

// TenantDueQR handles GET /tenant/dues/:id/qr.
func (h *Handlers) TenantDueQR(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
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
	h.serveDueQR(c, false)
}

// TenantPayments handles GET /tenant/payments.
func (h *Handlers) TenantPayments(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	ctx := c.Request.Context()
	list, err := h.PaymentStore.ListByTenant(ctx, t.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}

	type TenantPaymentResponseItem struct {
		domain.Payment
		UIStatus      string `json:"ui_status,omitempty"`
		StatusMessage string `json:"status_message,omitempty"`
	}

	knownPaymentIDs := make(map[uuid.UUID]bool)
	respPayments := make([]TenantPaymentResponseItem, 0, len(list))
	for _, p := range list {
		knownPaymentIDs[p.ID] = true
		item := TenantPaymentResponseItem{Payment: p}
		if p.IsUnapplied {
			item.UIStatus = "HELD_UNAPPLIED"
			item.StatusMessage = domain.TenantUIStatusHeld
		}
		respPayments = append(respPayments, item)
	}

	// Also surface held/resolving unmatched receipts that have not created a payment row yet (e.g. amount mismatch)
	var heldReceipts []domain.UnmatchedGatewayReceipt
	if h.UnmatchedReceipts != nil {
		receipts, err := h.UnmatchedReceipts.ListUnmatchedReceiptsByTenant(ctx, t.ID)
		if err == nil {
			for _, r := range receipts {
				if r.Status == domain.ReceiptStatusHeld || r.Status == domain.ReceiptStatusResolving {
					heldReceipts = append(heldReceipts, r)
					if r.PaymentID == nil || !knownPaymentIDs[*r.PaymentID] {
						var cfPaymentID *string
						if r.ProviderPaymentID != "" {
							cfPaymentID = &r.ProviderPaymentID
						}
						respPayments = append(respPayments, TenantPaymentResponseItem{
							Payment: domain.Payment{
								ID:          r.ID,
								TenantID:    t.ID,
								Amount:      r.AmountPaise,
								Provider:    r.Provider,
								CFPaymentID: cfPaymentID,
								IsUnapplied: true,
								CreatedAt:   r.CreatedAt,
							},
							UIStatus:      "HELD_UNAPPLIED",
							StatusMessage: domain.TenantUIStatusHeld,
						})
					}
				}
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"payments":           respPayments,
		"unapplied_receipts": heldReceipts,
	})
}

// TenantPaymentReceipt handles GET /tenant/payments/:id/receipt (read-only receipt payload).
func (h *Handlers) TenantPaymentReceipt(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	id, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	if h.PaymentStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "payments unavailable"})
		return
	}
	p, err := h.PaymentStore.GetByID(c.Request.Context(), id)
	if err != nil || p == nil || p.TenantID != t.ID {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"receipt": gin.H{
			"payment_id":   p.ID,
			"due_id":       p.DueID,
			"tenant_id":    p.TenantID,
			"amount_paise": p.Amount,
			"matched_at":   p.MatchedAt,
			"matched_by":   p.MatchedBy,
			"upi_txn_id":   p.UPITxnID,
			"property_id":  p.PropertyID,
		},
	})
}

// TenantLifecycle handles GET /tenant/lifecycle (deposit and departure read-only status).
func (h *Handlers) TenantLifecycle(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	out := gin.H{
		"tenant_status": t.Status,
		"credit_balance_paise": t.CreditBalancePaise,
	}
	if h.PayoutRepo != nil {
		dep, err := h.PayoutRepo.GetActiveDepartureByTenant(c.Request.Context(), t.ID)
		if err == nil && dep != nil {
			out["departure"] = dep
		}
	}
	c.JSON(http.StatusOK, out)
}

// TenantPushSubscribe handles POST /tenant/push/subscribe.
func (h *Handlers) TenantPushSubscribe(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	var body pushSubBody
	if err := c.ShouldBindJSON(&body); err != nil {
		apierr.RespondBindErr(c, "invalid body", apierr.CodeRequestInvalidBody)
		return
	}
	if err := h.Push.Subscribe(c.Request.Context(), t.ID, body.Endpoint, body.Keys.P256dh, body.Keys.Auth); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "subscribe failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

type aadhaarBody struct {
	QRPayload string `json:"qr_payload"`
	Name      string `json:"name"`
	DOB       string `json:"dob"`
	Gender    string `json:"gender"`
	UIDLast4  string `json:"uid_last4"`
	Consent   bool   `json:"consent" binding:"required"`
	Channel   string `json:"channel"`
	Confirm   bool   `json:"confirm"`
}

// TenantAadhaar handles POST /tenant/aadhaar.
func (h *Handlers) TenantAadhaar(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	var body aadhaarBody
	if err := c.ShouldBindJSON(&body); err != nil || !body.Consent {
		c.JSON(http.StatusBadRequest, gin.H{"error": "consent required"})
		return
	}
	channel := body.Channel
	if channel == "" {
		channel = "tenant_app"
	}

	data := aadhaar.AadhaarData{
		Name:     body.Name,
		DOB:      body.DOB,
		Gender:   body.Gender,
		UIDLast4: body.UIDLast4,
	}
	partial := true
	fromQR := body.QRPayload != ""
	if fromQR {
		decoded, p, err := aadhaar.DecodeAadhaarQR(body.QRPayload)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "qr decode failed — use manual last-4 if the card has no Secure QR"})
			return
		}
		partial = p
		data = decoded
		if !body.Confirm {
			c.JSON(http.StatusOK, gin.H{
				"partial":       partial,
				"needs_confirm": true,
				"name":          data.Name,
				"dob":           data.DOB,
				"yob":           data.YOB,
				"gender":        data.Gender,
				"uid_last4":     data.UIDLast4,
				"verified":      data.Verified,
			})
			return
		}
		if body.Name != "" {
			data.Name = body.Name
		}
	}

	last4 := aadhaar.PersistableLast4(fromQR, data.Verified, data.UIDLast4, body.UIDLast4)
	if last4 != "" {
		if !aadhaarLast4Pattern.MatchString(last4) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "uid_last4 must be exactly 4 digits"})
			return
		}
		if err := aadhaar.GuardOverwrite(t.AadhaarLast4, last4); err != nil {
			c.JSON(http.StatusConflict, gin.H{"error": "aadhaar last-4 already set"})
			return
		}
		if t.AadhaarLast4 == nil || *t.AadhaarLast4 != last4 {
			cloned := *t
			cloned.AadhaarLast4 = &last4
			if err := h.TenantStore.Update(c.Request.Context(), &cloned); err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
				return
			}
			t.AadhaarLast4 = &last4
			auth.InvalidateTenantCache(t.ID)
		}
	}

	if h.Events != nil {
		_ = aadhaar.RecordConsent(c.Request.Context(), h.Events, t.ID, t.PropertyID, channel, "aadhaar_kyc")
	}

	c.JSON(http.StatusOK, gin.H{
		"partial":       partial,
		"name":          data.Name,
		"dob":           data.DOB,
		"yob":           data.YOB,
		"gender":        data.Gender,
		"uid_last4":     data.UIDLast4,
		"verified":      data.Verified,
		"aadhaar_last4": t.AadhaarLast4,
	})
}

func tenantFromContext(c *gin.Context) *domain.Tenant {
	v, ok := c.Get(auth.ContextTenantKey)
	if !ok {
		c.JSON(http.StatusForbidden, gin.H{"error": "access revoked"})
		return nil
	}
	t, ok := v.(*domain.Tenant)
	if !ok || t == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "access revoked"})
		return nil
	}
	return t
}
