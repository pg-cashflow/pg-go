package finance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

// Close semantics back the DB period lock (migration 044, control C-4): a period may only close
// after it has ended, closing is idempotent, and a closed period is a frozen snapshot.

func newTieOutSvc(now time.Time) (*Service, uuid.UUID) {
	svc := NewService(NewMemoryStore(), nil)
	svc.Now = func() time.Time { return now }
	return svc, uuid.New()
}

func TestCloseTieOutRejectsPeriodNotEnded(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		now  time.Time
	}{
		{"mid period", time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)},
		{"last second of period", time.Date(2026, 9, 30, 23, 59, 59, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, pid := newTieOutSvc(tc.now)
			if _, err := svc.ComputeTieOut(ctx, pid, "2026-09", &payment.ReconciliationSummary{Period: "2026-09"}); err != nil {
				t.Fatalf("compute: %v", err)
			}
			_, err := svc.CloseTieOut(ctx, pid, "2026-09")
			if !errors.Is(err, ErrPeriodNotCloseable) {
				t.Fatalf("expected ErrPeriodNotCloseable, got %v", err)
			}
			got, _ := svc.Store.GetTieOut(ctx, pid, "2026-09")
			if got.Status != "open" || got.ClosedAt != nil {
				t.Fatalf("period must stay open, got status=%s closedAt=%v", got.Status, got.ClosedAt)
			}
		})
	}
}

func TestCloseTieOutAtPeriodEndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	// First UTC instant after the period: PeriodBounds' end is exclusive, so this must be allowed.
	svc, pid := newTieOutSvc(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))
	if _, err := svc.ComputeTieOut(ctx, pid, "2026-09", &payment.ReconciliationSummary{Period: "2026-09"}); err != nil {
		t.Fatalf("compute: %v", err)
	}
	first, err := svc.CloseTieOut(ctx, pid, "2026-09")
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if first.Status != "closed" || first.ClosedAt == nil {
		t.Fatalf("expected closed with ClosedAt, got status=%s closedAt=%v", first.Status, first.ClosedAt)
	}
	svc.Now = func() time.Time { return time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC) }
	second, err := svc.CloseTieOut(ctx, pid, "2026-09")
	if err != nil {
		t.Fatalf("second close must be a no-op, got %v", err)
	}
	if !second.ClosedAt.Equal(*first.ClosedAt) {
		t.Fatalf("second close rewrote ClosedAt: %v -> %v", first.ClosedAt, second.ClosedAt)
	}
}

func TestComputeTieOutClosedPeriodIsFrozen(t *testing.T) {
	ctx := context.Background()
	svc, pid := newTieOutSvc(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	if _, err := svc.ComputeTieOut(ctx, pid, "2026-09", &payment.ReconciliationSummary{Period: "2026-09"}); err != nil {
		t.Fatalf("compute: %v", err)
	}
	if _, err := svc.CloseTieOut(ctx, pid, "2026-09"); err != nil {
		t.Fatalf("close: %v", err)
	}
	// GET /finance/tie-out recomputes: it must return the closing snapshot, not rewrite it.
	got, err := svc.ComputeTieOut(ctx, pid, "2026-09", &payment.ReconciliationSummary{RentCollected: 5000, Period: "2026-09"})
	if err != nil {
		t.Fatalf("recompute on closed period: %v", err)
	}
	if got.Status != "closed" || got.ReconTotalPaise != 0 || got.DifferencePaise != 0 {
		t.Fatalf("closed period was modified: status=%s recon=%d diff=%d", got.Status, got.ReconTotalPaise, got.DifferencePaise)
	}
	stored, _ := svc.Store.GetTieOut(ctx, pid, "2026-09")
	if stored.ReconTotalPaise != 0 || stored.Status != "closed" {
		t.Fatalf("stored row changed: %+v", stored)
	}
}

func TestCloseTieOutStillBlocksUnexplainedDifferenceBeforeEndCheck(t *testing.T) {
	ctx := context.Background()
	svc, pid := newTieOutSvc(time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC))
	if _, err := svc.ComputeTieOut(ctx, pid, "2026-09", &payment.ReconciliationSummary{RentCollected: 1000, Period: "2026-09"}); err != nil {
		t.Fatalf("compute: %v", err)
	}
	if _, err := svc.CloseTieOut(ctx, pid, "2026-09"); err != ErrPeriodNotCloseable {
		t.Fatalf("an unexplained difference must still return the bare sentinel, got %v", err)
	}
}
