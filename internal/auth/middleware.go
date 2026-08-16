package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

const (
	ContextClaimsKey = "claims"
	ContextTenantKey = "tenant"
)

// ClaimsFromContext returns JWT claims previously set by RequireOwner / RequireTenant.
func ClaimsFromContext(c *gin.Context) (*Claims, bool) {
	v, ok := c.Get(ContextClaimsKey)
	if !ok {
		return nil, false
	}
	claims, ok := v.(*Claims)
	return claims, ok
}

// RequireOwner validates the Bearer JWT and requires role=owner.
func RequireOwner(jwtSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := authenticate(c, jwtSecret)
		if !ok {
			return
		}
		if claims.Role != domain.RoleOwner {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
		c.Set(ContextClaimsKey, claims)
		c.Next()
	}
}

// RequireTenant validates the Bearer JWT, requires role=tenant, and checks live
// tenant.status == active via TenantRepo.GetByID. Vacated tenants get 403
// "access revoked" — a 30-day JWT alone is not enough to stay logged in.
func RequireTenant(jwtSecret string, tenantRepo TenantRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := authenticate(c, jwtSecret)
		if !ok {
			return
		}
		if claims.Role != domain.RoleTenant {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
		if claims.TenantID == nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "access revoked"})
			return
		}
		tenant, err := tenantRepo.GetByID(c.Request.Context(), *claims.TenantID)
		if err != nil || tenant.Status != domain.TenantStatusActive {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "access revoked"})
			return
		}
		c.Set(ContextClaimsKey, claims)
		c.Set(ContextTenantKey, tenant)
		c.Next()
	}
}

func authenticate(c *gin.Context, jwtSecret string) (*Claims, bool) {
	header := c.GetHeader("Authorization")
	if header == "" || !strings.HasPrefix(header, "Bearer ") {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing bearer token"})
		return nil, false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	claims, err := VerifyToken(jwtSecret, raw)
	if err != nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid token"})
		return nil, false
	}
	return claims, true
}
