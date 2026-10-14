package auth

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// CapabilityChecker reads active manager grants.
type CapabilityChecker interface {
	HasActive(ctx context.Context, propertyID, userID uuid.UUID, capability string) (bool, error)
}

type capHit struct {
	ok bool
	at time.Time
}

// RequireCapability allows the owner. A manager must hold the named grant.
// A nil checker does not enforce grants. cmd/server always sets a checker.
// Database errors fail closed with HTTP 503. Grants are cached for 30 seconds.
func RequireCapability(cap string, checker CapabilityChecker) gin.HandlerFunc {
	var mu sync.Mutex
	cache := map[string]capHit{}
	return func(c *gin.Context) {
		if checker == nil {
			c.Next()
			return
		}
		claims, ok := ClaimsFromContext(c)
		if !ok {
			apierr.Abort(c, http.StatusUnauthorized, "unauthorized", apierr.CodeAuthUnauthorized)
			return
		}
		if claims.Role == domain.RoleOwner {
			c.Next()
			return
		}
		if claims.Role != domain.RoleManager || claims.PropertyID == nil {
			apierr.Abort(c, http.StatusForbidden, "forbidden", apierr.CodeAuthForbidden)
			return
		}
		key := claims.PropertyID.String() + "|" + claims.UserID.String() + "|" + cap
		mu.Lock()
		hit, found := cache[key]
		if found && time.Since(hit.at) < 30*time.Second {
			mu.Unlock()
			if !hit.ok {
				apierr.Abort(c, http.StatusForbidden, "forbidden", apierr.CodeAuthForbidden)
				return
			}
			c.Next()
			return
		}
		mu.Unlock()

		allowed, err := checker.HasActive(c.Request.Context(), *claims.PropertyID, claims.UserID, cap)
		if err != nil {
			apierr.Abort(c, http.StatusServiceUnavailable, "capability lookup failed", apierr.CodeAuthForbidden)
			return
		}
		mu.Lock()
		cache[key] = capHit{ok: allowed, at: time.Now()}
		mu.Unlock()
		if !allowed {
			apierr.Abort(c, http.StatusForbidden, "forbidden", apierr.CodeAuthForbidden)
			return
		}
		c.Next()
	}
}
