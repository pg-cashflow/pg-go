package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var delegableCapabilities = []string{
	"tenants.view",
	"dues.view",
	"dues.mark_cash_paid",
	"expenses.post",
	"expenses.void",
	"attendance.mark",
	"inspections.write",
	"meters.write",
	"kitchen.headcount",
	"complaints.respond",
	"notifications.send",
	"slot_invites.create",
	"slot_invites.allow_over_capacity",
	"refunds.request",
}

func isDelegableCapability(cap string) bool {
	for _, c := range delegableCapabilities {
		if c == cap {
			return true
		}
	}
	return false
}

const (
	ProfileFrontDesk       = "front_desk"
	ProfileOperations      = "operations"
	ProfileFinanceAssistant = "finance_assistant"
	ProfileCustom          = "custom"
)

var profileCapabilities = map[string][]string{
	ProfileFrontDesk: {
		"tenants.view",
		"dues.view",
		"complaints.respond",
	},
	ProfileOperations: {
		"attendance.mark",
		"inspections.write",
		"meters.write",
		"kitchen.headcount",
		"complaints.respond",
	},
	ProfileFinanceAssistant: {
		"tenants.view",
		"dues.view",
		"expenses.post",
		"refunds.request",
	},
	ProfileCustom: {},
}

func isValidProfile(p string) bool {
	switch p {
	case ProfileFrontDesk, ProfileOperations, ProfileFinanceAssistant, ProfileCustom:
		return true
	default:
		return false
	}
}

func stringSliceDifference(a, b []string) []string {
	bMap := make(map[string]bool, len(b))
	for _, item := range b {
		bMap[item] = true
	}
	var diff []string
	for _, item := range a {
		if !bMap[item] {
			diff = append(diff, item)
		}
	}
	if diff == nil {
		diff = []string{}
	}
	return diff
}

func nullIfEmptyStr(s string) *string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func (h *Handlers) getPool() *pgxpool.Pool {
	if h.AppPool != nil {
		return h.AppPool
	}
	return h.Pool
}

// OwnerCreateManager provisions a warden/manager user account scoped to a property.
func (h *Handlers) OwnerCreateManager(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	var body struct {
		PropertyID   string   `json:"property_id"`
		Phone        string   `json:"phone" binding:"required"`
		Email        string   `json:"email,omitempty"`
		Profile      string   `json:"profile,omitempty"`
		Capabilities []string `json:"capabilities,omitempty"`
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

	profile := strings.TrimSpace(body.Profile)
	if profile == "" {
		if len(body.Capabilities) > 0 {
			profile = ProfileCustom
		} else {
			profile = ProfileFrontDesk
		}
	}
	if !isValidProfile(profile) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid profile: must be front_desk, operations, finance_assistant, or custom"})
		return
	}

	var capsToGrant []string
	if profile == ProfileCustom {
		for _, cap := range body.Capabilities {
			if !isDelegableCapability(cap) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "capability not delegable: " + cap})
				return
			}
		}
		capsToGrant = body.Capabilities
	} else {
		capsToGrant = profileCapabilities[profile]
	}

	u := &domain.User{
		Phone:      normPhone,
		Email:      cleanEmail,
		Role:       domain.RoleManager,
		PropertyID: &pid,
		Profile:    profile,
	}
	existing, _ := h.UserStore.GetByPhone(c.Request.Context(), normPhone)
	if existing != nil {
		if existing.Role != domain.RoleManager {
			c.JSON(http.StatusConflict, gin.H{
				"error": "account already exists with a different role; automatic conversion is denied",
				"code":  apierr.CodeAuthForbidden,
			})
			return
		}
		if existing.PropertyID == nil || *existing.PropertyID != pid {
			c.JSON(http.StatusConflict, gin.H{
				"error": "manager already assigned to another property; cross-property transfer is denied",
				"code":  apierr.CodeAuthForbidden,
			})
			return
		}
		// Existing manager on this property: return existing without blindly reactivating revoked capabilities
		c.JSON(http.StatusOK, gin.H{"manager": existing})
		return
	}

	actorID, _ := userIDFromClaims(c)
	pool := h.getPool()
	if pool != nil {
		ctx := c.Request.Context()
		tx, err := pool.Begin(ctx)
		if err != nil {
			respondErr(c, err)
			return
		}
		defer tx.Rollback(ctx)

		var newID uuid.UUID
		var createdAt time.Time
		err = tx.QueryRow(ctx, `
			INSERT INTO users (phone, email, role, property_id, profile, created_at)
			VALUES ($1, $2, $3, $4, $5, NOW())
			RETURNING id, created_at`,
			normPhone, nullIfEmptyStr(cleanEmail), domain.RoleManager, pid, profile,
		).Scan(&newID, &createdAt)
		if err != nil {
			respondErr(c, err)
			return
		}
		u.ID = newID
		u.CreatedAt = createdAt

		// Level-1 lock on new user row
		var lockedID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, newID).Scan(&lockedID); err != nil {
			respondErr(c, err)
			return
		}

		for _, cap := range capsToGrant {
			_, err = tx.Exec(ctx, `
				INSERT INTO manager_capabilities (property_id, user_id, capability, granted_by, granted_at, revoked_at)
				VALUES ($1, $2, $3, $4, NOW(), NULL)
				ON CONFLICT (property_id, user_id, capability) DO UPDATE
				SET granted_by = EXCLUDED.granted_by, granted_at = NOW(), revoked_at = NULL`,
				pid, newID, cap, actorID,
			)
			if err != nil {
				respondErr(c, err)
				return
			}
		}

		outboxPayload, _ := json.Marshal(map[string]any{
			"manager_id":   newID.String(),
			"phone":        normPhone,
			"profile":      profile,
			"capabilities": capsToGrant,
		})
		_, err = tx.Exec(ctx, `
			INSERT INTO security_events_outbox (property_id, actor_id, target_user_id, event_type, payload, status)
			VALUES ($1, $2, $3, $4, $5, 'pending')`,
			pid, actorID, newID, "manager.created", outboxPayload,
		)
		if err != nil {
			respondErr(c, err)
			return
		}

		if err := tx.Commit(ctx); err != nil {
			respondErr(c, err)
			return
		}
	} else {
		if err := h.UserStore.Create(c.Request.Context(), u); err != nil {
			respondErr(c, err)
			return
		}

		if h.CapabilityStore != nil {
			for _, cap := range capsToGrant {
				if err := h.CapabilityStore.Grant(c.Request.Context(), pid, u.ID, cap, actorID); err != nil {
					respondErr(c, err)
					return
				}
			}
		}
		h.publishManagerAudit(c, pid, u.ID, "created", map[string]any{
			"profile":      profile,
			"capabilities": capsToGrant,
		})
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
		entry := gin.H{
			"id":          m.ID,
			"phone":       m.Phone,
			"email":       m.Email,
			"profile":     m.Profile,
			"disabled_at": m.DisabledAt,
			"created_at":  m.CreatedAt,
		}
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
	if h.CapabilityStore == nil && h.getPool() == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "capability store unavailable"})
		return
	}
	var body struct {
		Profile string   `json:"profile,omitempty"`
		Grant   []string `json:"grant"`
		Revoke  []string `json:"revoke"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}

	for _, cap := range body.Revoke {
		if !isDelegableCapability(cap) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "capability not delegable: " + cap})
			return
		}
	}
	for _, cap := range body.Grant {
		if !isDelegableCapability(cap) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "capability not delegable: " + cap})
			return
		}
	}

	targetProfile := strings.TrimSpace(body.Profile)
	if targetProfile != "" && !isValidProfile(targetProfile) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid profile: must be front_desk, operations, finance_assistant, or custom"})
		return
	}
	if targetProfile == "" {
		targetProfile = ProfileCustom
	}

	grantedBy, _ := userIDFromClaims(c)
	pool := h.getPool()
	if pool != nil {
		ctx := c.Request.Context()
		tx, err := pool.Begin(ctx)
		if err != nil {
			respondErr(c, err)
			return
		}
		defer tx.Rollback(ctx)

		// Level-1 Lock on manager user row
		var userRole string
		var userPropID *uuid.UUID
		var disabledAt *time.Time
		err = tx.QueryRow(ctx, `
			SELECT role, property_id, disabled_at
			FROM users
			WHERE id = $1
			FOR UPDATE`, managerID,
		).Scan(&userRole, &userPropID, &disabledAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"error": "manager not found"})
				return
			}
			respondErr(c, err)
			return
		}
		if userRole != string(domain.RoleManager) || userPropID == nil || *userPropID != pid {
			c.JSON(http.StatusNotFound, gin.H{"error": "manager not found"})
			return
		}
		if disabledAt != nil {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "cannot modify capabilities of deactivated manager",
				"code":  apierr.CodeAuthForbidden,
			})
			return
		}

		for _, cap := range body.Revoke {
			_, err = tx.Exec(ctx, `
				UPDATE manager_capabilities SET revoked_at = NOW()
				WHERE property_id = $1 AND user_id = $2 AND capability = $3 AND revoked_at IS NULL`,
				pid, managerID, cap,
			)
			if err != nil {
				respondErr(c, err)
				return
			}
		}

		for _, cap := range body.Grant {
			_, err = tx.Exec(ctx, `
				INSERT INTO manager_capabilities (property_id, user_id, capability, granted_by, granted_at, revoked_at)
				VALUES ($1, $2, $3, $4, NOW(), NULL)
				ON CONFLICT (property_id, user_id, capability) DO UPDATE
				SET granted_by = EXCLUDED.granted_by, granted_at = NOW(), revoked_at = NULL`,
				pid, managerID, cap, grantedBy,
			)
			if err != nil {
				respondErr(c, err)
				return
			}
		}

		_, err = tx.Exec(ctx, `
			UPDATE users 
			SET profile = $1, token_version = token_version + 1
			WHERE id = $2`, targetProfile, managerID,
		)
		if err != nil {
			respondErr(c, err)
			return
		}

		outboxPayload, _ := json.Marshal(map[string]any{
			"manager_id": managerID.String(),
			"grant":      body.Grant,
			"revoke":     body.Revoke,
			"profile":    targetProfile,
		})
		_, err = tx.Exec(ctx, `
			INSERT INTO security_events_outbox (property_id, actor_id, target_user_id, event_type, payload, status)
			VALUES ($1, $2, $3, $4, $5, 'pending')`,
			pid, grantedBy, managerID, "manager.capabilities_updated", outboxPayload,
		)
		if err != nil {
			respondErr(c, err)
			return
		}

		if err := tx.Commit(ctx); err != nil {
			respondErr(c, err)
			return
		}

		auth.InvalidateTokenVersionCache(managerID)
	} else {
		mgr, err := h.UserStore.GetByID(c.Request.Context(), managerID)
		if err != nil || mgr == nil || mgr.Role != domain.RoleManager || mgr.PropertyID == nil || *mgr.PropertyID != pid {
			c.JSON(http.StatusNotFound, gin.H{"error": "manager not found"})
			return
		}
		if mgr.IsDisabled() {
			c.JSON(http.StatusForbidden, gin.H{
				"error": "cannot modify capabilities of deactivated manager",
				"code":  apierr.CodeAuthForbidden,
			})
			return
		}
		for _, cap := range body.Revoke {
			if err := h.CapabilityStore.Revoke(c.Request.Context(), pid, managerID, cap); err != nil {
				respondErr(c, err)
				return
			}
		}
		for _, cap := range body.Grant {
			if err := h.CapabilityStore.Grant(c.Request.Context(), pid, managerID, cap, grantedBy); err != nil {
				respondErr(c, err)
				return
			}
		}
		mgr.Profile = targetProfile
		if h.AuthUserRepo != nil {
			_ = h.AuthUserRepo.IncrementTokenVersion(c.Request.Context(), managerID)
		}
		auth.InvalidateTokenVersionCache(managerID)
		h.publishManagerAudit(c, pid, managerID, "capabilities_updated", body)
	}

	var caps []string
	if h.CapabilityStore != nil {
		caps, _ = h.CapabilityStore.ListActive(c.Request.Context(), pid, managerID)
	} else if pool != nil {
		rows, _ := pool.Query(c.Request.Context(), `
			SELECT capability FROM manager_capabilities
			WHERE property_id = $1 AND user_id = $2 AND revoked_at IS NULL
			ORDER BY capability`, pid, managerID)
		if rows != nil {
			defer rows.Close()
			for rows.Next() {
				var cap string
				if err := rows.Scan(&cap); err == nil {
					caps = append(caps, cap)
				}
			}
		}
	}
	if caps == nil {
		caps = []string{}
	}
	c.JSON(http.StatusOK, gin.H{"capabilities": caps, "profile": targetProfile})
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
	pool := h.getPool()
	if pool != nil {
		ctx := c.Request.Context()
		actorID, _ := userIDFromClaims(c)
		tx, err := pool.Begin(ctx)
		if err != nil {
			respondErr(c, err)
			return
		}
		defer tx.Rollback(ctx)

		var role string
		var propID *uuid.UUID
		err = tx.QueryRow(ctx, `
			SELECT role, property_id
			FROM users
			WHERE id = $1
			FOR UPDATE`, managerID,
		).Scan(&role, &propID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"error": "manager not found"})
				return
			}
			respondErr(c, err)
			return
		}
		if role != string(domain.RoleManager) || propID == nil || *propID != pid {
			c.JSON(http.StatusNotFound, gin.H{"error": "manager not found"})
			return
		}

		_, err = tx.Exec(ctx, `
			UPDATE users
			SET disabled_at = NULL, token_version = token_version + 1
			WHERE id = $1`, managerID,
		)
		if err != nil {
			respondErr(c, err)
			return
		}

		outboxPayload, _ := json.Marshal(map[string]any{
			"manager_id": managerID.String(),
			"action":     "reactivated",
		})
		_, err = tx.Exec(ctx, `
			INSERT INTO security_events_outbox (property_id, actor_id, target_user_id, event_type, payload, status)
			VALUES ($1, $2, $3, $4, $5, 'pending')`,
			pid, actorID, managerID, "manager.reactivated", outboxPayload,
		)
		if err != nil {
			respondErr(c, err)
			return
		}

		if err := tx.Commit(ctx); err != nil {
			respondErr(c, err)
			return
		}

		auth.InvalidateTokenVersionCache(managerID)
		c.JSON(http.StatusOK, gin.H{"ok": true, "note": "re-grant capabilities via PUT /owner/managers/:id/capabilities"})
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
	mgr.DisabledAt = nil
	if h.AuthUserRepo != nil {
		_ = h.AuthUserRepo.IncrementTokenVersion(c.Request.Context(), managerID)
	}
	auth.InvalidateTokenVersionCache(managerID)
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
	pool := h.getPool()
	if pool != nil {
		ctx := c.Request.Context()
		actorID, _ := userIDFromClaims(c)
		tx, err := pool.Begin(ctx)
		if err != nil {
			respondErr(c, err)
			return
		}
		defer tx.Rollback(ctx)

		var role string
		var propID *uuid.UUID
		err = tx.QueryRow(ctx, `
			SELECT role, property_id
			FROM users
			WHERE id = $1
			FOR UPDATE`, managerID,
		).Scan(&role, &propID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"error": "manager not found"})
				return
			}
			respondErr(c, err)
			return
		}
		if role != string(domain.RoleManager) || propID == nil || *propID != pid {
			c.JSON(http.StatusNotFound, gin.H{"error": "manager not found"})
			return
		}

		_, err = tx.Exec(ctx, `UPDATE users SET token_version = token_version + 1 WHERE id = $1`, managerID)
		if err != nil {
			respondErr(c, err)
			return
		}

		outboxPayload, _ := json.Marshal(map[string]any{
			"manager_id": managerID.String(),
			"action":     "credentials_reset",
		})
		_, err = tx.Exec(ctx, `
			INSERT INTO security_events_outbox (property_id, actor_id, target_user_id, event_type, payload, status)
			VALUES ($1, $2, $3, $4, $5, 'pending')`,
			pid, actorID, managerID, "manager.credentials_reset", outboxPayload,
		)
		if err != nil {
			respondErr(c, err)
			return
		}

		if err := tx.Commit(ctx); err != nil {
			respondErr(c, err)
			return
		}

		auth.InvalidateTokenVersionCache(managerID)
		h.publishManagerAudit(c, pid, managerID, "credentials_reset", nil)
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	}

	if h.AuthUserRepo == nil || h.UserStore == nil {
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
	pool := h.getPool()
	if pool != nil {
		ctx := c.Request.Context()
		actorID, _ := userIDFromClaims(c)
		tx, err := pool.Begin(ctx)
		if err != nil {
			respondErr(c, err)
			return false
		}
		defer tx.Rollback(ctx)

		var role string
		var propID *uuid.UUID
		err = tx.QueryRow(ctx, `
			SELECT role, property_id
			FROM users
			WHERE id = $1
			FOR UPDATE`, managerID,
		).Scan(&role, &propID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"error": "manager not found"})
				return false
			}
			respondErr(c, err)
			return false
		}
		if role != string(domain.RoleManager) || propID == nil || *propID != pid {
			c.JSON(http.StatusNotFound, gin.H{"error": "manager not found"})
			return false
		}

		_, err = tx.Exec(ctx, `
			UPDATE manager_capabilities SET revoked_at = NOW()
			WHERE property_id = $1 AND user_id = $2 AND revoked_at IS NULL`,
			pid, managerID,
		)
		if err != nil {
			respondErr(c, err)
			return false
		}

		_, err = tx.Exec(ctx, `
			UPDATE users
			SET disabled_at = NOW(), token_version = token_version + 1
			WHERE id = $1`, managerID,
		)
		if err != nil {
			respondErr(c, err)
			return false
		}

		outboxPayload, _ := json.Marshal(map[string]any{
			"manager_id": managerID.String(),
			"action":     "deactivated",
		})
		_, err = tx.Exec(ctx, `
			INSERT INTO security_events_outbox (property_id, actor_id, target_user_id, event_type, payload, status)
			VALUES ($1, $2, $3, $4, $5, 'pending')`,
			pid, actorID, managerID, "manager.deactivated", outboxPayload,
		)
		if err != nil {
			respondErr(c, err)
			return false
		}

		if err := tx.Commit(ctx); err != nil {
			respondErr(c, err)
			return false
		}

		auth.InvalidateTokenVersionCache(managerID)
		h.publishManagerAudit(c, pid, managerID, "deactivated", nil)
		return true
	}

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
	now := time.Now().UTC()
	mgr.DisabledAt = &now
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

// ManagerAuditResult contains deep consistency audit details for a single manager.
type ManagerAuditResult struct {
	ManagerID             uuid.UUID `json:"manager_id"`
	Phone                 string    `json:"phone"`
	Email                 string    `json:"email,omitempty"`
	Profile               string    `json:"profile"`
	IsDisabled            bool      `json:"is_disabled"`
	ActualCapabilities    []string  `json:"actual_capabilities"`
	ExpectedCapabilities  []string  `json:"expected_capabilities"`
	MissingCapabilities   []string  `json:"missing_capabilities"`
	UnexpectedCapabilities []string  `json:"unexpected_capabilities"`
	HasDrift              bool      `json:"has_drift"`
	DriftReason           string    `json:"drift_reason,omitempty"`
}

// ManagerAuditSummary aggregates audit findings for the property.
type ManagerAuditSummary struct {
	TotalManagers int                  `json:"total_managers"`
	CleanCount    int                  `json:"clean_count"`
	DriftCount    int                  `json:"drift_count"`
	Audits        []ManagerAuditResult `json:"audits"`
}

// OwnerAuditManagers handles GET /owner/managers/audit.
// It compares actual active capabilities against expected profile configurations.
func (h *Handlers) OwnerAuditManagers(c *gin.Context) {
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

	summary := ManagerAuditSummary{
		TotalManagers: len(managers),
		Audits:        make([]ManagerAuditResult, 0, len(managers)),
	}

	for _, m := range managers {
		var actualCaps []string
		if h.CapabilityStore != nil {
			actualCaps, _ = h.CapabilityStore.ListActive(c.Request.Context(), pid, m.ID)
		} else if pool := h.getPool(); pool != nil {
			rows, _ := pool.Query(c.Request.Context(), `
				SELECT capability FROM manager_capabilities
				WHERE property_id = $1 AND user_id = $2 AND revoked_at IS NULL
				ORDER BY capability`, pid, m.ID)
			if rows != nil {
				defer rows.Close()
				for rows.Next() {
					var cap string
					if err := rows.Scan(&cap); err == nil {
						actualCaps = append(actualCaps, cap)
					}
				}
			}
		}
		if actualCaps == nil {
			actualCaps = []string{}
		}

		res := auditSingleManager(m, actualCaps)
		if res.HasDrift {
			summary.DriftCount++
		} else {
			summary.CleanCount++
		}
		summary.Audits = append(summary.Audits, res)
	}

	c.JSON(http.StatusOK, summary)
}

func auditSingleManager(m domain.User, actualCaps []string) ManagerAuditResult {
	res := ManagerAuditResult{
		ManagerID:          m.ID,
		Phone:              m.Phone,
		Email:              m.Email,
		Profile:            m.Profile,
		IsDisabled:         m.IsDisabled(),
		ActualCapabilities: actualCaps,
	}

	sort.Strings(actualCaps)

	if res.IsDisabled {
		res.ExpectedCapabilities = []string{}
		res.MissingCapabilities = []string{}
		res.UnexpectedCapabilities = actualCaps
		if len(actualCaps) > 0 {
			res.HasDrift = true
			res.DriftReason = "deactivated_manager_has_active_grants"
		}
		return res
	}

	if m.Profile == ProfileCustom {
		if len(actualCaps) == 0 {
			res.ExpectedCapabilities = []string{}
			res.MissingCapabilities = []string{}
			res.UnexpectedCapabilities = []string{}
			res.HasDrift = true
			res.DriftReason = "custom_manager_has_zero_capabilities"
			return res
		}
		nonDelegable := stringSliceDifference(actualCaps, delegableCapabilities)
		if len(nonDelegable) > 0 {
			res.ExpectedCapabilities = delegableCapabilities
			res.MissingCapabilities = []string{}
			res.UnexpectedCapabilities = nonDelegable
			res.HasDrift = true
			res.DriftReason = "manager_has_non_delegable_capabilities"
			return res
		}
		res.ExpectedCapabilities = actualCaps
		res.MissingCapabilities = []string{}
		res.UnexpectedCapabilities = []string{}
		res.HasDrift = false
		return res
	}

	expected, ok := profileCapabilities[m.Profile]
	if !ok {
		res.ExpectedCapabilities = []string{}
		res.MissingCapabilities = []string{}
		res.UnexpectedCapabilities = actualCaps
		res.HasDrift = true
		res.DriftReason = "invalid_profile"
		return res
	}

	sort.Strings(expected)
	res.ExpectedCapabilities = expected
	res.MissingCapabilities = stringSliceDifference(expected, actualCaps)
	res.UnexpectedCapabilities = stringSliceDifference(actualCaps, expected)

	if len(res.MissingCapabilities) > 0 || len(res.UnexpectedCapabilities) > 0 {
		res.HasDrift = true
		res.DriftReason = "profile_capability_mismatch"
	}
	return res
}

// OwnerRepairManager handles POST /owner/managers/:id/repair.
// It repairs permission drift for a manager, synchronizing grants to their profile.
func (h *Handlers) OwnerRepairManager(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	managerID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	var body struct {
		TargetProfile string   `json:"target_profile,omitempty"`
		Capabilities  []string `json:"capabilities,omitempty"`
	}
	_ = c.ShouldBindJSON(&body)

	targetProfile := strings.TrimSpace(body.TargetProfile)
	if targetProfile != "" && !isValidProfile(targetProfile) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid target_profile"})
		return
	}

	actorID, _ := userIDFromClaims(c)
	pool := h.getPool()
	if pool != nil {
		ctx := c.Request.Context()
		tx, err := pool.Begin(ctx)
		if err != nil {
			respondErr(c, err)
			return
		}
		defer tx.Rollback(ctx)

		// Level-1 Lock
		var role string
		var propID *uuid.UUID
		var currentProfile string
		var disabledAt *time.Time
		err = tx.QueryRow(ctx, `
			SELECT role, property_id, profile, disabled_at
			FROM users
			WHERE id = $1
			FOR UPDATE`, managerID,
		).Scan(&role, &propID, &currentProfile, &disabledAt)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				c.JSON(http.StatusNotFound, gin.H{"error": "manager not found"})
				return
			}
			respondErr(c, err)
			return
		}
		if role != string(domain.RoleManager) || propID == nil || *propID != pid {
			c.JSON(http.StatusNotFound, gin.H{"error": "manager not found"})
			return
		}

		if disabledAt != nil {
			_, err = tx.Exec(ctx, `
				UPDATE manager_capabilities SET revoked_at = NOW()
				WHERE property_id = $1 AND user_id = $2 AND revoked_at IS NULL`,
				pid, managerID,
			)
			if err != nil {
				respondErr(c, err)
				return
			}

			_, err = tx.Exec(ctx, `UPDATE users SET token_version = token_version + 1 WHERE id = $1`, managerID)
			if err != nil {
				respondErr(c, err)
				return
			}

			outboxPayload, _ := json.Marshal(map[string]any{
				"manager_id": managerID.String(),
				"action":     "repaired_deactivated",
			})
			_, err = tx.Exec(ctx, `
				INSERT INTO security_events_outbox (property_id, actor_id, target_user_id, event_type, payload, status)
				VALUES ($1, $2, $3, $4, $5, 'pending')`,
				pid, actorID, managerID, "manager.repaired", outboxPayload,
			)
			if err != nil {
				respondErr(c, err)
				return
			}

			if err := tx.Commit(ctx); err != nil {
				respondErr(c, err)
				return
			}
			auth.InvalidateTokenVersionCache(managerID)
			c.JSON(http.StatusOK, gin.H{
				"ok":           true,
				"is_disabled":  true,
				"profile":      currentProfile,
				"capabilities": []string{},
			})
			return
		}

		if targetProfile == "" {
			if isValidProfile(currentProfile) {
				targetProfile = currentProfile
			} else {
				targetProfile = ProfileFrontDesk
			}
		}

		var targetCaps []string
		if targetProfile == ProfileCustom {
			if len(body.Capabilities) > 0 {
				for _, cap := range body.Capabilities {
					if !isDelegableCapability(cap) {
						c.JSON(http.StatusBadRequest, gin.H{"error": "capability not delegable: " + cap})
						return
					}
				}
				targetCaps = body.Capabilities
			} else {
				targetProfile = ProfileFrontDesk
				targetCaps = profileCapabilities[ProfileFrontDesk]
			}
		} else {
			targetCaps = profileCapabilities[targetProfile]
		}

		_, err = tx.Exec(ctx, `
			UPDATE manager_capabilities SET revoked_at = NOW()
			WHERE property_id = $1 AND user_id = $2 AND revoked_at IS NULL`,
			pid, managerID,
		)
		if err != nil {
			respondErr(c, err)
			return
		}

		for _, cap := range targetCaps {
			_, err = tx.Exec(ctx, `
				INSERT INTO manager_capabilities (property_id, user_id, capability, granted_by, granted_at, revoked_at)
				VALUES ($1, $2, $3, $4, NOW(), NULL)
				ON CONFLICT (property_id, user_id, capability) DO UPDATE
				SET granted_by = EXCLUDED.granted_by, granted_at = NOW(), revoked_at = NULL`,
				pid, managerID, cap, actorID,
			)
			if err != nil {
				respondErr(c, err)
				return
			}
		}

		_, err = tx.Exec(ctx, `
			UPDATE users 
			SET profile = $1, token_version = token_version + 1
			WHERE id = $2`, targetProfile, managerID,
		)
		if err != nil {
			respondErr(c, err)
			return
		}

		outboxPayload, _ := json.Marshal(map[string]any{
			"manager_id":   managerID.String(),
			"profile":      targetProfile,
			"capabilities": targetCaps,
			"action":       "repaired_active",
		})
		_, err = tx.Exec(ctx, `
			INSERT INTO security_events_outbox (property_id, actor_id, target_user_id, event_type, payload, status)
			VALUES ($1, $2, $3, $4, $5, 'pending')`,
			pid, actorID, managerID, "manager.repaired", outboxPayload,
		)
		if err != nil {
			respondErr(c, err)
			return
		}

		if err := tx.Commit(ctx); err != nil {
			respondErr(c, err)
			return
		}
		auth.InvalidateTokenVersionCache(managerID)
		c.JSON(http.StatusOK, gin.H{
			"ok":           true,
			"is_disabled":  false,
			"profile":      targetProfile,
			"capabilities": targetCaps,
		})
		return
	}

	if h.UserStore == nil || h.CapabilityStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "service unavailable"})
		return
	}
	mgr, err := h.UserStore.GetByID(c.Request.Context(), managerID)
	if err != nil || mgr == nil || mgr.Role != domain.RoleManager || mgr.PropertyID == nil || *mgr.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "manager not found"})
		return
	}

	if mgr.IsDisabled() {
		_ = h.CapabilityStore.RevokeAll(c.Request.Context(), pid, managerID)
		if h.AuthUserRepo != nil {
			_ = h.AuthUserRepo.IncrementTokenVersion(c.Request.Context(), managerID)
		}
		auth.InvalidateTokenVersionCache(managerID)
		c.JSON(http.StatusOK, gin.H{
			"ok":           true,
			"is_disabled":  true,
			"profile":      mgr.Profile,
			"capabilities": []string{},
		})
		return
	}

	if targetProfile == "" {
		if isValidProfile(mgr.Profile) {
			targetProfile = mgr.Profile
		} else {
			targetProfile = ProfileFrontDesk
		}
	}
	targetCaps := profileCapabilities[targetProfile]
	_ = h.CapabilityStore.RevokeAll(c.Request.Context(), pid, managerID)
	for _, cap := range targetCaps {
		_ = h.CapabilityStore.Grant(c.Request.Context(), pid, managerID, cap, actorID)
	}
	mgr.Profile = targetProfile
	if h.AuthUserRepo != nil {
		_ = h.AuthUserRepo.IncrementTokenVersion(c.Request.Context(), managerID)
	}
	auth.InvalidateTokenVersionCache(managerID)
	c.JSON(http.StatusOK, gin.H{
		"ok":           true,
		"is_disabled":  false,
		"profile":      targetProfile,
		"capabilities": targetCaps,
	})
}
