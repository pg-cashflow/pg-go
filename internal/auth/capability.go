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

const (
	capabilityCacheTTL        = 30 * time.Second
	maxCapabilityCacheEntries = 1024
)

// RequireAnyCapability allows the owner. A manager must hold at least one of the named grants.
// A nil checker does not enforce grants. cmd/server always sets a checker.
// Database errors fail closed with HTTP 503. Grants are cached for 30 seconds.
// Memory is bounded to maxCapabilityCacheEntries with active eviction.
func RequireAnyCapability(checker CapabilityChecker, caps ...string) gin.HandlerFunc {
	if checker == nil {
		panic("auth: RequireAnyCapability requires a non-nil CapabilityChecker")
	}
	var mu sync.Mutex
	cache := make(map[string]capHit)
	return func(c *gin.Context) {
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

		now := time.Now()
		var allowed bool
		for _, cap := range caps {
			key := claims.PropertyID.String() + "|" + claims.UserID.String() + "|" + cap
			mu.Lock()
			hit, found := cache[key]
			if found && now.Sub(hit.at) < capabilityCacheTTL {
				mu.Unlock()
				if hit.ok {
					allowed = true
					break
				}
				continue
			}
			mu.Unlock()

			hasCap, err := checker.HasActive(c.Request.Context(), *claims.PropertyID, claims.UserID, cap)
			if err != nil {
				apierr.Abort(c, http.StatusServiceUnavailable, "capability lookup failed", apierr.CodeAuthForbidden)
				return
			}

			mu.Lock()
			if len(cache) >= maxCapabilityCacheEntries {
				for k, v := range cache {
					if now.Sub(v.at) >= capabilityCacheTTL {
						delete(cache, k)
					}
				}
				if len(cache) >= maxCapabilityCacheEntries {
					var oldestKey string
					var oldestTime time.Time
					first := true
					for k, v := range cache {
						if first || v.at.Before(oldestTime) {
							oldestKey = k
							oldestTime = v.at
							first = false
						}
					}
					if !first {
						delete(cache, oldestKey)
					}
				}
			}
			cache[key] = capHit{ok: hasCap, at: now}
			mu.Unlock()

			if hasCap {
				allowed = true
				break
			}
		}

		if !allowed {
			apierr.Abort(c, http.StatusForbidden, "forbidden", apierr.CodeAuthForbidden)
			return
		}
		c.Next()
	}
}

// RequireCapability allows the owner. A manager must hold the named grant.
func RequireCapability(cap string, checker CapabilityChecker) gin.HandlerFunc {
	return RequireAnyCapability(checker, cap)
}

