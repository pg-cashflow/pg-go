package auth

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

const (
	ContextClaimsKey = "claims"
	ContextTenantKey = "tenant"
)

// ClaimsFromContext extracts *Claims set by any auth middleware.
func ClaimsFromContext(c *gin.Context) (*Claims, bool) {
	v, ok := c.Get(ContextClaimsKey)
	if !ok {
		return nil, false
	}
	claims, ok := v.(*Claims)
	return claims, ok
}

// RequireOwner validates the Bearer JWT and requires role=owner.
func RequireOwner(jwtSecret string, users UserLookup) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := authenticate(c, jwtSecret, users)
		if !ok {
			return
		}
		if claims.Role != domain.RoleOwner {
			apierr.Abort(c, http.StatusForbidden, "forbidden", apierr.CodeAuthForbidden)
			return
		}
		c.Set(ContextClaimsKey, claims)
		c.Next()
	}
}

// RequireManagerOrOwner validates the Bearer JWT and requires role=owner or role=manager.
func RequireManagerOrOwner(jwtSecret string, users UserLookup) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := authenticate(c, jwtSecret, users)
		if !ok {
			return
		}
		if claims.Role != domain.RoleOwner && claims.Role != domain.RoleManager {
			apierr.Abort(c, http.StatusForbidden, "forbidden", apierr.CodeAuthForbidden)
			return
		}
		c.Set(ContextClaimsKey, claims)
		c.Next()
	}
}

// RequireOwnerOrManagerOrTenant validates the Bearer JWT and accepts any
// authenticated role. Used for endpoints that are role-agnostic but still
// require a valid session (e.g. /notifications, which scopes data by user_id).
func RequireOwnerOrManagerOrTenant(jwtSecret string, users UserLookup) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := authenticate(c, jwtSecret, users)
		if !ok {
			return
		}
		c.Set(ContextClaimsKey, claims)
		c.Next()
	}
}

// RequireTenant validates the Bearer JWT, requires role=tenant, and checks live
// tenant.status is active or pending_allocation via TenantRepo.GetByID.
// Vacated tenants get 403 "access revoked" — a 30-day JWT alone is not enough.
func RequireTenant(jwtSecret string, tenantRepo TenantRepository, users UserLookup) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := authenticate(c, jwtSecret, users)
		if !ok {
			return
		}
		if claims.Role != domain.RoleTenant {
			apierr.Abort(c, http.StatusForbidden, "forbidden", apierr.CodeAuthForbidden)
			return
		}
		if claims.TenantID == nil {
			apierr.Abort(c, http.StatusForbidden, "complete your profile to continue", apierr.CodeAuthProfileIncomplete)
			return
		}
		tenant, err := tenantRepo.GetByID(c.Request.Context(), *claims.TenantID)
		if err != nil {
			apierr.Abort(c, http.StatusForbidden, "access revoked", apierr.CodeAuthAccessRevoked)
			return
		}
		switch tenant.Status {
		case domain.TenantStatusActive, domain.TenantStatusPendingAllocation:
			c.Set(ContextClaimsKey, claims)
			c.Set(ContextTenantKey, tenant)
			c.Next()
		default:
			apierr.Abort(c, http.StatusForbidden, "access revoked", apierr.CodeAuthAccessRevoked)
		}
	}
}

// RequirePendingJoin allows a tenant JWT that is not yet linked to a tenant row (invite wait).
func RequirePendingJoin(jwtSecret string, users UserLookup) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims, ok := authenticate(c, jwtSecret, users)
		if !ok {
			return
		}
		if claims.Role != domain.RoleTenant {
			apierr.Abort(c, http.StatusForbidden, "forbidden", apierr.CodeAuthForbidden)
			return
		}
		if claims.TenantID != nil {
			apierr.Abort(c, http.StatusForbidden, "already activated", apierr.CodeAuthAlreadyActivated)
			return
		}
		if claims.PropertyID == nil {
			apierr.Abort(c, http.StatusForbidden, "forbidden", apierr.CodeAuthForbidden)
			return
		}
		c.Set(ContextClaimsKey, claims)
		c.Next()
	}
}

func authenticate(c *gin.Context, jwtSecret string, users UserLookup) (*Claims, bool) {
	header := c.GetHeader("Authorization")
	if header == "" || !strings.HasPrefix(header, "Bearer ") {
		apierr.Abort(c, http.StatusUnauthorized, "missing bearer token", apierr.CodeAuthMissingToken)
		return nil, false
	}
	raw := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	claims, err := VerifyToken(jwtSecret, raw)
	if err != nil {
		apierr.Abort(c, http.StatusUnauthorized, "invalid token", apierr.CodeAuthInvalidToken)
		return nil, false
	}
	if !sessionStillValid(c, users, claims) {
		return nil, false
	}
	return claims, true
}

func sessionStillValid(c *gin.Context, users UserLookup, claims *Claims) bool {
	if users == nil {
		return true
	}
	u, err := users.GetByID(c.Request.Context(), claims.UserID)
	if err != nil || u == nil {
		apierr.Abort(c, http.StatusForbidden, "access revoked", apierr.CodeAuthAccessRevoked)
		return false
	}
	live := tokenVersionOf(u)
	claimTV := claims.TokenVersion
	if claimTV < DefaultTokenVersion {
		claimTV = DefaultTokenVersion
	}
	if claimTV < live {
		apierr.Abort(c, http.StatusForbidden, "access revoked", apierr.CodeAuthAccessRevoked)
		return false
	}
	return true
}
