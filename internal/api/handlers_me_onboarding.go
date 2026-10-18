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
	kycVerified := false
	if claims.TenantID != nil {
		kycVerified = h.IsKYCVerified(c.Request.Context(), *claims.TenantID)
	}
	state, err := h.Joins.BuildOnboardingState(c.Request.Context(), user, kycVerified)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "onboarding state failed"})
		return
	}
	c.JSON(http.StatusOK, state)
}
