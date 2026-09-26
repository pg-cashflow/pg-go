package api

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
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

func ipRateLimitBounded(rps float64, burst int, maxEntries int, ttl time.Duration) gin.HandlerFunc {
	var mu sync.Mutex
	limiters := make(map[string]*ipLimiterEntry)

	sweep := func(now time.Time) {
		for ip, e := range limiters {
			if now.Sub(e.lastSeen) > ttl {
				delete(limiters, ip)
			}
		}
	}

	return func(c *gin.Context) {
		ip := c.ClientIP()
		now := time.Now()

		mu.Lock()
		e, ok := limiters[ip]
		if !ok {
			if len(limiters) >= maxEntries {
				sweep(now)
			}
			if len(limiters) >= maxEntries {
				// Evict oldest entry if still at or above capacity
				var oldestIP string
				var oldestTime time.Time
				for k, v := range limiters {
					if oldestIP == "" || v.lastSeen.Before(oldestTime) {
						oldestIP = k
						oldestTime = v.lastSeen
					}
				}
				if oldestIP != "" {
					delete(limiters, oldestIP)
				}
			}
			e = &ipLimiterEntry{
				limiter:  rate.NewLimiter(rate.Limit(rps), burst),
				lastSeen: now,
			}
			limiters[ip] = e
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
