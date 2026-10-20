package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var delegableCapabilities = []string{
	"tenants.view", "dues.view", "dues.mark_cash_paid",
	"expenses.post", "expenses.void", "attendance.mark",
	"inspections.write", "meters.write", "kitchen.headcount",
	"complaints.respond", "notifications.send",
	"slot_invites.create", "slot_invites.allow_over_capacity",
}

func isDelegableCapability(cap string) bool {
	for _, c := range delegableCapabilities {
		if c == cap {
			return true
		}
	}
	return false
}

// OwnerCreateManager provisions a warden/manager user account scoped to a property.
func (h *Handlers) OwnerCreateManager(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	var body struct {
		PropertyID string `json:"property_id"`
		Phone      string `json:"phone" binding:"required"`
		Email      string `json:"email,omitempty"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Phone == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone is required"})
		return
	}

	if body.PropertyID != "" {
		reqPID, err := uuid.Parse(body.PropertyID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": "invalid property_id"})
			return
		}
		if reqPID != pid {
			authorized := false
			if h.PropertyStore != nil {
				if prop, err := h.PropertyStore.GetByID(c.Request.Context(), reqPID); err == nil && prop != nil {
					claims, _ := auth.ClaimsFromContext(c)
					var userPhone, userEmail string
					if h.UserStore != nil {
						if u, _ := h.UserStore.GetByID(c.Request.Context(), claims.UserID); u != nil {
							userPhone = u.Phone
							userEmail = u.Email
						}
					}
					if userPhone != "" && prop.OwnerPhone == userPhone {
						authorized = true
					} else if userEmail != "" && strings.EqualFold(prop.OwnerEmail, userEmail) {
						authorized = true
					}
				}
			}
			if !authorized {
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "not authorized for this property"})
				return
			}
			pid = reqPID
		}
	}

	normPhone := auth.NormalizePhone(body.Phone)
	if normPhone == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid phone number format"})
		return
	}

	cleanEmail := strings.ToLower(strings.TrimSpace(body.Email))
	if cleanEmail != "" && (!strings.Contains(cleanEmail, "@") || !strings.Contains(cleanEmail, ".")) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid email format"})
		return
	}

	u := &domain.User{
		Phone:      normPhone,
		Email:      cleanEmail,
		Role:       domain.RoleManager,
		PropertyID: &pid,
	}
	existing, _ := h.UserStore.GetByPhone(c.Request.Context(), normPhone)
	if existing != nil {
		u = existing
		if u.Role != domain.RoleManager || u.PropertyID == nil || *u.PropertyID != pid {
			u.Role = domain.RoleManager
			u.PropertyID = &pid
			_ = h.UserStore.SetPropertyID(c.Request.Context(), u.ID, pid)
		}
	} else {
		if err := h.UserStore.Create(c.Request.Context(), u); err != nil {
			respondErr(c, err)
			return
		}
	}

	if h.CapabilityStore != nil {
		grantedBy, _ := userIDFromClaims(c)
		for _, cap := range delegableCapabilities {
			_ = h.CapabilityStore.Grant(c.Request.Context(), pid, u.ID, cap, grantedBy)
		}
	}
	c.JSON(http.StatusOK, gin.H{"manager": u})
}

// OwnerListManagers handles GET /owner/managers.
func (h *Handlers) OwnerListManagers(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	if h.UserStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user store unavailable"})
		return
	}
	managers, err := h.UserStore.GetByPropertyAndRole(c.Request.Context(), pid, domain.RoleManager)
	if err != nil {
		respondErr(c, err)
		return
	}
	out := make([]gin.H, 0, len(managers))
	for _, m := range managers {
		entry := gin.H{"id": m.ID, "phone": m.Phone, "created_at": m.CreatedAt}
		if h.CapabilityStore != nil {
			caps, _ := h.CapabilityStore.ListActive(c.Request.Context(), pid, m.ID)
			entry["capabilities"] = caps
		}
		out = append(out, entry)
	}
	c.JSON(http.StatusOK, gin.H{"managers": out})
}

// OwnerPutManagerCapabilities handles PUT /owner/managers/:id/capabilities.
func (h *Handlers) OwnerPutManagerCapabilities(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	managerID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	if h.CapabilityStore == nil || h.UserStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "capability store unavailable"})
		return
	}
	mgr, err := h.UserStore.GetByID(c.Request.Context(), managerID)
	if err != nil || mgr == nil || mgr.Role != domain.RoleManager || mgr.PropertyID == nil || *mgr.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "manager not found"})
		return
	}
	var body struct {
		Grant  []string `json:"grant"`
		Revoke []string `json:"revoke"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	grantedBy, _ := userIDFromClaims(c)
	for _, cap := range body.Revoke {
		if !isDelegableCapability(cap) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "capability not delegable"})
			return
		}
		if err := h.CapabilityStore.Revoke(c.Request.Context(), pid, managerID, cap); err != nil {
			respondErr(c, err)
			return
		}
	}
	for _, cap := range body.Grant {
		if !isDelegableCapability(cap) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "capability not delegable"})
			return
		}
		if err := h.CapabilityStore.Grant(c.Request.Context(), pid, managerID, cap, grantedBy); err != nil {
			respondErr(c, err)
			return
		}
	}
	h.publishManagerAudit(c, pid, managerID, "capabilities_updated", body)
	caps, _ := h.CapabilityStore.ListActive(c.Request.Context(), pid, managerID)
	c.JSON(http.StatusOK, gin.H{"capabilities": caps})
}

// OwnerDeactivateManager handles POST /owner/managers/:id/deactivate.
func (h *Handlers) OwnerDeactivateManager(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	managerID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	if !h.deactivateManager(c, pid, managerID) {
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// OwnerReactivateManager handles POST /owner/managers/:id/reactivate.
func (h *Handlers) OwnerReactivateManager(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	managerID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	if h.UserStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user store unavailable"})
		return
	}
	mgr, err := h.UserStore.GetByID(c.Request.Context(), managerID)
	if err != nil || mgr == nil || mgr.Role != domain.RoleManager || mgr.PropertyID == nil || *mgr.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "manager not found"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "note": "re-grant capabilities via PUT /owner/managers/:id/capabilities"})
}

// OwnerResetManagerCredentials handles POST /owner/managers/:id/reset-credentials.
func (h *Handlers) OwnerResetManagerCredentials(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	managerID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	if h.AuthUserRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "session store unavailable"})
		return
	}
	mgr, err := h.UserStore.GetByID(c.Request.Context(), managerID)
	if err != nil || mgr == nil || mgr.Role != domain.RoleManager || mgr.PropertyID == nil || *mgr.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "manager not found"})
		return
	}
	if err := h.AuthUserRepo.IncrementTokenVersion(c.Request.Context(), managerID); err != nil {
		respondErr(c, err)
		return
	}
	auth.InvalidateTokenVersionCache(managerID)
	h.publishManagerAudit(c, pid, managerID, "credentials_reset", nil)
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func (h *Handlers) deactivateManager(c *gin.Context, pid, managerID uuid.UUID) bool {
	if h.UserStore == nil || h.CapabilityStore == nil || h.AuthUserRepo == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service unavailable"})
		return false
	}
	mgr, err := h.UserStore.GetByID(c.Request.Context(), managerID)
	if err != nil || mgr == nil || mgr.Role != domain.RoleManager || mgr.PropertyID == nil || *mgr.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "manager not found"})
		return false
	}
	if err := h.CapabilityStore.RevokeAll(c.Request.Context(), pid, managerID); err != nil {
		respondErr(c, err)
		return false
	}
	if err := h.AuthUserRepo.IncrementTokenVersion(c.Request.Context(), managerID); err != nil {
		respondErr(c, err)
		return false
	}
	auth.InvalidateTokenVersionCache(managerID)
	h.publishManagerAudit(c, pid, managerID, "deactivated", nil)
	return true
}

func (h *Handlers) publishManagerAudit(c *gin.Context, propertyID, managerID uuid.UUID, action string, detail interface{}) {
	if h.Events == nil {
		return
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"manager_id": managerID.String(),
		"action":     action,
		"detail":     detail,
	})
	evtType := domain.EvtManagerCapabilityChanged
	if action == "deactivated" {
		evtType = domain.EvtManagerDeactivated
	}
	_ = h.Events.Publish(c.Request.Context(), domain.Event{
		PropertyID: propertyID,
		EventType:  evtType,
		Payload:    payload,
	})
}
