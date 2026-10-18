package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// IsKYCVerified queries the latest KYC verification state for a tenant directly from the database.
// This prevents relying on stale claims or cached tokens.
func (h *Handlers) IsKYCVerified(ctx context.Context, tenantID uuid.UUID) bool {
	if h.KYCSvc == nil {
		return false
	}
	ver, _, err := h.KYCSvc.GetStatus(ctx, tenantID)
	return err == nil && ver != nil && ver.Status == domain.KYCStatusVerified
}

// RequireKYCVerified returns a middleware that requires the authenticated tenant
// to have completed KYC verification (status == domain.KYCStatusVerified).
func (h *Handlers) RequireKYCVerified() gin.HandlerFunc {
	return func(c *gin.Context) {
		t := tenantFromContext(c)
		if t == nil {
			return
		}
		if h.KYCSvc == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, apierr.ErrorEnvelope{Error: "kyc service not configured"})
			return
		}
		if !h.IsKYCVerified(c.Request.Context(), t.ID) {
			apierr.Abort(c, http.StatusForbidden, "kyc verification required", apierr.CodeAuthForbidden)
			return
		}
		c.Next()
	}
}

// RequireKYCIfPolicyEnforced enforces KYC verification only if the tenant's property
// has RequireKYCBeforePay (or onboarding KYC enforcement) enabled in its settings.
func (h *Handlers) RequireKYCIfPolicyEnforced() gin.HandlerFunc {
	return func(c *gin.Context) {
		t := tenantFromContext(c)
		if t == nil {
			return
		}
		requireKYC := false
		if h.OnboardingSettings != nil {
			s, err := h.OnboardingSettings.Get(c.Request.Context(), t.PropertyID)
			if err == nil && s != nil {
				requireKYC = s.RequireKYCBeforePay
			}
		}
		if !requireKYC {
			c.Next()
			return
		}
		if h.KYCSvc == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, apierr.ErrorEnvelope{Error: "kyc service not configured"})
			return
		}
		if !h.IsKYCVerified(c.Request.Context(), t.ID) {
			apierr.Abort(c, http.StatusForbidden, "kyc verification required", apierr.CodeAuthForbidden)
			return
		}
		c.Next()
	}
}
