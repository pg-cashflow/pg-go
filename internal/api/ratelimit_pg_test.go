package api

import (
	"context"
	"testing"
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
