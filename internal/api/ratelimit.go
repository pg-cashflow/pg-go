package api

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"golang.org/x/time/rate"
)

type ipLimiterEntry struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

const (
	defaultMaxIPEntries = 10000
	defaultEntryTTL     = 10 * time.Minute
)

// ipRateLimit returns a per-IP Gin middleware using a token-bucket algorithm.
//
// rps is the sustained request rate (tokens per second); burst is the maximum
// instantaneous allowance. Example: ipRateLimit(3.0/60, 5) allows 3 req/min
// sustained with a burst of 5.
//
// Memory bounding: tracks up to maxIPEntries unique IPs with a 10-minute TTL.
// When capacity is reached, expired entries are purged; if still full, the oldest
// entry is evicted.
func ipRateLimit(rps float64, burst int) gin.HandlerFunc {
	return ipRateLimitBounded(rps, burst, defaultMaxIPEntries, defaultEntryTTL)
}

func keyedRateLimitBounded(rps float64, burst int, maxEntries int, ttl time.Duration, keyFn func(*gin.Context) string) gin.HandlerFunc {
	var mu sync.Mutex
	limiters := make(map[string]*ipLimiterEntry)

	sweep := func(now time.Time) {
		for k, e := range limiters {
			if now.Sub(e.lastSeen) > ttl {
				delete(limiters, k)
			}
		}
	}

	return func(c *gin.Context) {
		key := keyFn(c)
		now := time.Now()

		mu.Lock()
		e, ok := limiters[key]
		if !ok {
			if len(limiters) >= maxEntries {
				sweep(now)
			}
			if len(limiters) >= maxEntries {
				// Evict oldest entry if still at or above capacity
				var oldestKey string
				var oldestTime time.Time
				for k, v := range limiters {
					if oldestKey == "" || v.lastSeen.Before(oldestTime) {
						oldestKey = k
						oldestTime = v.lastSeen
					}
				}
				if oldestKey != "" {
					delete(limiters, oldestKey)
				}
			}
			e = &ipLimiterEntry{
				limiter:  rate.NewLimiter(rate.Limit(rps), burst),
				lastSeen: now,
			}
			limiters[key] = e
		} else {
			e.lastSeen = now
		}
		allowed := e.limiter.Allow()
		mu.Unlock()

		if !allowed {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
			return
		}
		c.Next()
	}
}

func ipRateLimitBounded(rps float64, burst int, maxEntries int, ttl time.Duration) gin.HandlerFunc {
	return keyedRateLimitBounded(rps, burst, maxEntries, ttl, func(c *gin.Context) string {
		return c.ClientIP()
	})
}

// userOrIPRateLimit returns a token-bucket rate limiter that keys on the authenticated
// user ID (claims.UserID) to prevent mobile carrier NAT IP sharing collisions.
// If unauthenticated, it safely falls back to ClientIP.
func userOrIPRateLimit(rps float64, burst int) gin.HandlerFunc {
	return userOrIPRateLimitBounded(rps, burst, defaultMaxIPEntries, defaultEntryTTL)
}

func userOrIPRateLimitBounded(rps float64, burst int, maxEntries int, ttl time.Duration) gin.HandlerFunc {
	return keyedRateLimitBounded(rps, burst, maxEntries, ttl, func(c *gin.Context) string {
		if claims, ok := auth.ClaimsFromContext(c); ok && claims.UserID != uuid.Nil {
			return "usr:" + claims.UserID.String()
		}
		return c.ClientIP()
	})
}
