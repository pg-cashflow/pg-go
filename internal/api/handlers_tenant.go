package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/aadhaar"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

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
	c.JSON(http.StatusOK, gin.H{"dues": list})
}

// TenantDueQR handles GET /tenant/dues/:id/qr.
func (h *Handlers) TenantDueQR(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
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
	list, err := h.PaymentStore.ListByTenant(c.Request.Context(), t.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "list failed"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"payments": list})
}

// TenantPushSubscribe handles POST /tenant/push/subscribe.
func (h *Handlers) TenantPushSubscribe(c *gin.Context) {
	t := tenantFromContext(c)
	if t == nil {
		return
	}
	var body pushSubBody
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
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
	if body.QRPayload != "" {
		decoded, p, err := aadhaar.DecodeAadhaarQR(body.QRPayload)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "qr decode failed"})
			return
		}
		partial = p
		if decoded.Name != "" {
			data.Name = decoded.Name
		}
		if decoded.DOB != "" {
			data.DOB = decoded.DOB
		}
		if decoded.Gender != "" {
			data.Gender = decoded.Gender
		}
		if decoded.UIDLast4 != "" {
			data.UIDLast4 = decoded.UIDLast4
		}
	}

	if data.UIDLast4 != "" {
		last4 := data.UIDLast4
		t.AadhaarLast4 = &last4
		if err := h.TenantStore.Update(c.Request.Context(), t); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update failed"})
			return
		}
	}

	if h.Events != nil {
		_ = aadhaar.RecordConsent(c.Request.Context(), h.Events, t.ID, t.PropertyID, channel, "aadhaar_kyc")
	}

	c.JSON(http.StatusOK, gin.H{
		"partial":       partial,
		"name":          data.Name,
		"dob":           data.DOB,
		"gender":        data.Gender,
		"uid_last4":     data.UIDLast4,
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
