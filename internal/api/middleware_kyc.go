package api

import (
	"context"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
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

// KYCOnboardingGatePassed reports whether the tenant has done their part of identity
// verification well enough to proceed past onboarding into the dashboard.
//
// This is deliberately broader than IsKYCVerified: a tenant who has submitted a
// document for manual review (status pending_manual_review) has completed the
// onboarding action required of them — the remaining step (owner approval) is not
// something the tenant can accelerate, so blocking the dashboard on it would strand
// them with no path forward. Automatic methods (digilocker / aadhaar_otp / qr) only
// pass the gate once status == verified, since those are near-instant.
func (h *Handlers) KYCOnboardingGatePassed(ctx context.Context, tenantID uuid.UUID) bool {
	if h.KYCSvc == nil {
		return false
	}
	ver, _, err := h.KYCSvc.GetStatus(ctx, tenantID)
	if err != nil || ver == nil {
		return false
	}
	return ver.Status == domain.KYCStatusVerified || ver.Status == domain.KYCStatusPendingManualReview
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
			if err != nil {
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, apierr.ErrorEnvelope{Error: "onboarding settings unavailable"})
				return
			}
			if s != nil {
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

// RequireKYCOnboardingPassed blocks tenant data routes until the tenant has completed their
// part of identity verification (verified, or pending owner review). It uses the same rule
// that yields the kyc_required onboarding stage, so the server and the dashboard gate agree.
// Fail-closed: a missing tenant, a missing KYC service, or a status lookup error all deny.
func (h *Handlers) RequireKYCOnboardingPassed() gin.HandlerFunc {
	return func(c *gin.Context) {
		v, ok := c.Get(auth.ContextTenantKey)
		t, isTenant := v.(*domain.Tenant)
		if !ok || !isTenant || t == nil {
			apierr.Abort(c, http.StatusForbidden, "access revoked", apierr.CodeAuthAccessRevoked)
			return
		}
		if !h.KYCOnboardingGatePassed(c.Request.Context(), t.ID) {
			apierr.Abort(c, http.StatusForbidden, "kyc verification required", apierr.CodeAuthForbidden)
			return
		}
		c.Next()
	}
}
