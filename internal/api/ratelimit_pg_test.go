package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPGRateLimiter_FailClosedOnNilPool(t *testing.T) {
	t.Run("nil receiver fails closed", func(t *testing.T) {
		var r *PGRateLimiter
		allowed, err := r.Allow(context.Background(), "test-key", 1.0, 5.0)
		if allowed {
			t.Error("expected allowed to be false for nil receiver")
		}
		if err == nil {
			t.Error("expected error for nil receiver, got nil")
		}
	})

	t.Run("nil pool fails closed", func(t *testing.T) {
		r := NewPGRateLimiter(nil)
		allowed, err := r.Allow(context.Background(), "test-key", 1.0, 5.0)
		if allowed {
			t.Error("expected allowed to be false for nil pool")
		}
		if err == nil {
			t.Error("expected error for nil pool, got nil")
		}
	})
}

func TestPGRateLimiter_MiddlewareStatusOnDBError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg, err := pgxpool.ParseConfig("postgres://dummy:dummy@127.0.0.1:54321/dummy?connect_timeout=1")
	if err != nil {
		t.Fatalf("failed to parse config: %v", err)
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}
	defer pool.Close()

	h := &Handlers{
		Deps: Deps{Pool: pool},
	}

	r := gin.New()
	r.GET("/ip-limited", h.IPRateLimit(10.0, 5), func(c *gin.Context) {
		c.String(http.StatusOK, "ok")
	})

	req := httptest.NewRequest(http.MethodGet, "/ip-limited", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected HTTP 503 on database failure, got %d (body: %s)", w.Code, w.Body.String())
	}
}
