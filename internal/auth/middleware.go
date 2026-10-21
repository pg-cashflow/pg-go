package auth

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
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

// AttachRequestPropertyScope binds the property ID from the verified token claims
// into the request context.
//
// It deliberately does NOT read X-Property-ID or property_id for any role. Owners
// may switch between properties, but that switch must be verified against the
// property store by ResolveOwnerPropertyScope (owner routes) before any handler
// runs. Trusting a request header here let an owner whose token carries no
// property_id read or write another property through routes that only call this
// middleware, such as the manager group.
func AttachRequestPropertyScope(c *gin.Context, claims *Claims) {
	if claims == nil || c == nil || c.Request == nil {
		return
	}
	if claims.PropertyID == nil || *claims.PropertyID == uuid.Nil {
		return
	}
	c.Request = c.Request.WithContext(requestscope.WithPropertyID(c.Request.Context(), *claims.PropertyID))
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
		tenantID := *claims.TenantID
		tenant, ok := getCachedTenant(tenantID)
		if !ok {
			var err error
			tenant, err = tenantRepo.GetByID(c.Request.Context(), tenantID)
			if err != nil {
				apierr.Abort(c, http.StatusForbidden, "access revoked", apierr.CodeAuthAccessRevoked)
				return
			}
			setCachedTenant(tenantID, tenant)
		}
		switch tenant.Status {
		case domain.TenantStatusActive, domain.TenantStatusPendingAllocation:
			c.Set(ContextClaimsKey, claims)
			c.Set(ContextTenantKey, tenant)
			c.Next()
		default:
			InvalidateTenantCache(tenantID)
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
	AttachRequestPropertyScope(c, claims)
	return claims, true
}

type tokenVersionHit struct {
	version   int
	expiresAt time.Time
}

var (
	tokenVersionMu    sync.RWMutex
	tokenVersionCache = make(map[uuid.UUID]tokenVersionHit)
)

// tokenVersionCacheTTL bounds how long a revoked access token keeps working
// after RevokeUserSessions / InvalidateTokenVersionCache is called elsewhere
// in the process. This is a deliberate latency-vs-load tradeoff, not an
// oversight: a 30s window means a force-logged-out user (password/session
// reset, fraud action) can keep making authenticated calls for up to 30s
// after revocation, in exchange for not hitting the users table on every
// request. Acceptable for this domain (PG tenant management, not a
// payments-authorization check) because money-moving actions (payout
// approval) sit behind a separate step-up OTP, not this cache. If a
// requirement ever needs sub-second revocation (e.g. a stolen-device kill
// switch), InvalidateTokenVersionCache already exists for an immediate,
// call-site-triggered eviction — shrinking the TTL is the wrong lever since
// it just trades latency back in for every request, not only revoked ones.
const tokenVersionCacheTTL = 30 * time.Second

// InvalidateTokenVersionCache evicts the cached token version for a user so
// subsequent requests force a fresh DB lookup.
func InvalidateTokenVersionCache(userID uuid.UUID) {
	tokenVersionMu.Lock()
	delete(tokenVersionCache, userID)
	tokenVersionMu.Unlock()
}

// ResetTokenVersionCache clears all cached token versions (primarily for testing).
func ResetTokenVersionCache() {
	tokenVersionMu.Lock()
	tokenVersionCache = make(map[uuid.UUID]tokenVersionHit)
	tokenVersionMu.Unlock()
}

func getCachedTokenVersion(userID uuid.UUID) (int, bool) {
	tokenVersionMu.RLock()
	hit, ok := tokenVersionCache[userID]
	tokenVersionMu.RUnlock()
	if ok && time.Now().Before(hit.expiresAt) {
		return hit.version, true
	}
	return 0, false
}

func setCachedTokenVersion(userID uuid.UUID, version int) {
	tokenVersionMu.Lock()
	tokenVersionCache[userID] = tokenVersionHit{
		version:   version,
		expiresAt: time.Now().Add(tokenVersionCacheTTL),
	}
	tokenVersionMu.Unlock()
}

type tenantCacheHit struct {
	tenant    *domain.Tenant
	expiresAt time.Time
}

var (
	tenantCacheMu sync.RWMutex
	tenantCache   = make(map[uuid.UUID]tenantCacheHit)
)

// tenantCacheTTL is the same deliberate tradeoff as tokenVersionCacheTTL
// above, applied to tenant.status reads in RequireTenant: a tenant marked
// `vacated` can keep hitting tenant routes for up to 30s before the cache
// expires or InvalidateTenantCache is called explicitly (e.g. from the
// vacate handler). Kept equal to tokenVersionCacheTTL so both caches expire
// together and this window has one documented value, not two.
const tenantCacheTTL = 30 * time.Second

// InvalidateTenantCache evicts the cached tenant for an ID so subsequent requests force a fresh DB lookup.
func InvalidateTenantCache(tenantID uuid.UUID) {
	tenantCacheMu.Lock()
	delete(tenantCache, tenantID)
	tenantCacheMu.Unlock()
}

// ResetTenantCache clears all cached tenants (primarily for testing).
func ResetTenantCache() {
	tenantCacheMu.Lock()
	tenantCache = make(map[uuid.UUID]tenantCacheHit)
	tenantCacheMu.Unlock()
}

func getCachedTenant(tenantID uuid.UUID) (*domain.Tenant, bool) {
	tenantCacheMu.RLock()
	hit, ok := tenantCache[tenantID]
	tenantCacheMu.RUnlock()
	if ok && time.Now().Before(hit.expiresAt) {
		return hit.tenant, true
	}
	return nil, false
}

func setCachedTenant(tenantID uuid.UUID, tenant *domain.Tenant) {
	tenantCacheMu.Lock()
	tenantCache[tenantID] = tenantCacheHit{
		tenant:    tenant,
		expiresAt: time.Now().Add(tenantCacheTTL),
	}
	tenantCacheMu.Unlock()
}

// StartCacheSweeper launches a background goroutine that periodically evicts expired
// entries from tokenVersionCache and tenantCache, bounding memory to O(active sessions).
func StartCacheSweeper(ctx context.Context, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				sweepExpiredCaches(now)
			}
		}
	}()
}

func sweepExpiredCaches(now time.Time) {
	tokenVersionMu.Lock()
	for k, v := range tokenVersionCache {
		if now.After(v.expiresAt) {
			delete(tokenVersionCache, k)
		}
	}
	tokenVersionMu.Unlock()

	tenantCacheMu.Lock()
	for k, v := range tenantCache {
		if now.After(v.expiresAt) {
			delete(tenantCache, k)
		}
	}
	tenantCacheMu.Unlock()
}

func sessionStillValid(c *gin.Context, users UserLookup, claims *Claims) bool {
	if users == nil {
		return true
	}

	claimTV := claims.TokenVersion
	if claimTV < DefaultTokenVersion {
		claimTV = DefaultTokenVersion
	}

	if live, ok := getCachedTokenVersion(claims.UserID); ok {
		if claimTV < live {
			apierr.Abort(c, http.StatusForbidden, "access revoked", apierr.CodeAuthAccessRevoked)
			return false
		}
		return true
	}

	u, err := users.GetByID(c.Request.Context(), claims.UserID)
	if err != nil || u == nil {
		apierr.Abort(c, http.StatusForbidden, "access revoked", apierr.CodeAuthAccessRevoked)
		return false
	}
	live := tokenVersionOf(u)
	setCachedTokenVersion(claims.UserID, live)

	if claimTV < live {
		apierr.Abort(c, http.StatusForbidden, "access revoked", apierr.CodeAuthAccessRevoked)
		return false
	}
	return true
}

