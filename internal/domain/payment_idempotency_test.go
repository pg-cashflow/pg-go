package domain

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestCashfreeRefundRecoveryIdempotencyKey(t *testing.T) {
	id := uuid.New()
	t0 := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	first := CashfreeRefundIdempotencyKey(id)
	a := CashfreeRefundRecoveryIdempotencyKey(id, t0)
	b := CashfreeRefundRecoveryIdempotencyKey(id, t0.Add(time.Second))
	if a == first || b == first || a == b {
		t.Fatalf("recovery keys must differ from the first key and from each other: %s %s %s", first, a, b)
	}
	if a != CashfreeRefundRecoveryIdempotencyKey(id, t0) {
		t.Fatal("recovery key must be deterministic for the same refund and instant")
	}
	if _, err := uuid.Parse(a); err != nil {
		t.Fatalf("not a UUID: %v", err)
	}
}
