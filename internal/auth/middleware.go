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

// RequireManagerOrOwner validates the Bearer JWT and requires role=owner or role=manager.
func RequireManagerOrOwner(jwtSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := authenticate(c, jwtSecret)
		if !ok {
			return
		}
		if claims.Role != domain.RoleOwner && claims.Role != domain.RoleManager {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
		c.Set(ContextClaimsKey, claims)
		c.Next()
	}
}


// RequireTenant validates the Bearer JWT, requires role=tenant, and checks live
// tenant.status is active or pending_allocation via TenantRepo.GetByID.
// Vacated tenants get 403 "access revoked" — a 30-day JWT alone is not enough.
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
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "complete your profile to continue"})
			return
		}
		tenant, err := tenantRepo.GetByID(c.Request.Context(), *claims.TenantID)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "access revoked"})
			return
		}
		switch tenant.Status {
		case domain.TenantStatusActive, domain.TenantStatusPendingAllocation:
			c.Set(ContextClaimsKey, claims)
			c.Set(ContextTenantKey, tenant)
			c.Next()
		default:
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "access revoked"})
		}
	}
}

// RequirePendingJoin allows a tenant JWT that is not yet linked to a tenant row (invite wait).
func RequirePendingJoin(jwtSecret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := authenticate(c, jwtSecret)
		if !ok {
			return
		}
		if claims.Role != domain.RoleTenant {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
		if claims.TenantID != nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "already activated"})
			return
		}
		if claims.PropertyID == nil {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden"})
			return
		}
		c.Set(ContextClaimsKey, claims)
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
