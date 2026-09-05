package api

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// ipRateLimit returns a per-IP Gin middleware using a token-bucket algorithm.
//
// rps is the sustained request rate (tokens per second); burst is the maximum
// instantaneous allowance. Example: ipRateLimit(3.0/60, 5) allows 3 req/min
// sustained with a burst of 5.
//
// Implementation note: limiters map grows by unique IP. This is acceptable at
// current scale. A TTL-eviction sweep should be added if cardinality becomes a
// concern (flagged in ADR-2 Batch 3 as deliberate scope cut).
func ipRateLimit(rps float64, burst int) gin.HandlerFunc {
	var mu sync.Mutex
	limiters := make(map[string]*rate.Limiter)

	return func(c *gin.Context) {
		ip := c.ClientIP()

		mu.Lock()
		l, ok := limiters[ip]
		if !ok {
			l = rate.NewLimiter(rate.Limit(rps), burst)
			limiters[ip] = l
		}
		mu.Unlock()

		if !l.Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
			return
		}
		c.Next()
	}
}
