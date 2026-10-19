package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// PGRateLimiter implements a persistent token-bucket rate limiter backed by PostgreSQL.
// It executes an atomic single-statement refill-and-consume operation on rate_limit_bucket.
// It fails closed: any database error denies the request.
type PGRateLimiter struct {
	pool *pgxpool.Pool
}

// NewPGRateLimiter constructs a new PostgreSQL rate limiter.
func NewPGRateLimiter(pool *pgxpool.Pool) *PGRateLimiter {
	return &PGRateLimiter{pool: pool}
}

// Allow checks and decrements tokens for key.
// Returns (true, nil) if allowed, (false, nil) if rate limited, or (false, err) on DB error.
func (r *PGRateLimiter) Allow(ctx context.Context, key string, rps float64, burst float64) (bool, error) {
	if r == nil || r.pool == nil {
		return false, errors.New("rate limiter: pool not configured")
	}
	const q = `
INSERT INTO rate_limit_bucket (bucket_key, tokens, updated_at)
VALUES ($1, $2 - 1, now())
ON CONFLICT (bucket_key) DO UPDATE SET
    tokens = LEAST($2, rate_limit_bucket.tokens
        + EXTRACT(EPOCH FROM (now() - rate_limit_bucket.updated_at)) * $3) - 1,
    updated_at = now()
WHERE LEAST($2, rate_limit_bucket.tokens
        + EXTRACT(EPOCH FROM (now() - rate_limit_bucket.updated_at)) * $3) >= 1
RETURNING tokens;`

	var tokens float64
	err := r.pool.QueryRow(ctx, q, key, burst, rps).Scan(&tokens)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // bucket had < 1 token — limit exceeded
	}
	if err != nil {
		return false, err // fail closed
	}
	return true, nil
}

// TenantIDRateLimit returns a middleware that applies durable PostgreSQL rate limiting
// keyed on tenant ID. If pool is nil (e.g. unit tests), it falls back to in-memory rate limiting.
func (h *Handlers) TenantIDRateLimit(rps float64, burst int) gin.HandlerFunc {
	if h != nil && h.Pool != nil {
		pgLimiter := NewPGRateLimiter(h.Pool)
		return func(c *gin.Context) {
			v, ok := c.Get(auth.ContextTenantKey)
			if !ok {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
				return
			}
			t, ok := v.(*domain.Tenant)
			if !ok || t == nil {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
				return
			}
			key := "tenant:" + t.ID.String() + ":" + c.FullPath()
			allowed, err := pgLimiter.Allow(c.Request.Context(), key, rps, float64(burst))
			if err != nil || !allowed {
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
				return
			}
			c.Next()
		}
	}
	return tenantIDRateLimit(rps, burst)
}

// IPRateLimit returns a middleware that applies durable PostgreSQL rate limiting keyed on client IP.
// If pool is nil, it falls back to in-memory rate limiting.
func (h *Handlers) IPRateLimit(rps float64, burst int) gin.HandlerFunc {
	if h != nil && h.Pool != nil {
		pgLimiter := NewPGRateLimiter(h.Pool)
		return func(c *gin.Context) {
			key := "ip:" + c.ClientIP() + ":" + c.FullPath()
			allowed, err := pgLimiter.Allow(c.Request.Context(), key, rps, float64(burst))
			if err != nil || !allowed {
				c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"error": "too many requests"})
				return
			}
			c.Next()
		}
	}
	return ipRateLimit(rps, burst)
}
