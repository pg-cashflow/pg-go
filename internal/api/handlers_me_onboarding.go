package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// MeOnboarding handles GET /me/onboarding — tenant admission timeline (P0-C).
func (h *Handlers) MeOnboarding(c *gin.Context) {
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		return
	}
	if h.Joins == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "onboarding unavailable"})
		return
	}
	user := &domain.User{
		ID:       claims.UserID,
		Role:     claims.Role,
		TenantID: claims.TenantID,
	}
	kycVerified := false   // true only once UIDAI/DigiLocker verification itself completed
	kycGatePassed := false // true once the tenant has done their part (includes pending_manual_review)
	if claims.TenantID != nil {
		kycVerified = h.IsKYCVerified(c.Request.Context(), *claims.TenantID)
		kycGatePassed = h.KYCOnboardingGatePassed(c.Request.Context(), *claims.TenantID)
	}
	state, err := h.Joins.BuildOnboardingState(c.Request.Context(), user, kycVerified, kycGatePassed)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "onboarding state failed"})
		return
	}
	c.JSON(http.StatusOK, state)
}
