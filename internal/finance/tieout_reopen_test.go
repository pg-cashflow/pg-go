package finance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

type capturingPublisher struct {
	events []domain.Event
}

func (p *capturingPublisher) Publish(_ context.Context, e domain.Event) error {
	p.events = append(p.events, e)
	return nil
}

func TestReopenTieOut_RejectsWhenNotClosed(t *testing.T) {
	ctx := context.Background()
	svc, pid := newTieOutSvc(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))

	// Period is not yet created or closed (status == "open")
	if _, err := svc.ComputeTieOut(ctx, pid, "2026-09", &payment.ReconciliationSummary{Period: "2026-09"}); err != nil {
		t.Fatalf("compute: %v", err)
	}

	_, err := svc.ReopenTieOut(ctx, pid, "2026-09", "owner_123", "reconcile audit findings")
	if !errors.Is(err, ErrPeriodNotReopenable) {
		t.Fatalf("expected ErrPeriodNotReopenable for open period, got: %v", err)
	}
}

func TestReopenTieOut_RequiresReason(t *testing.T) {
	ctx := context.Background()
	svc, pid := newTieOutSvc(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC))

	if _, err := svc.ComputeTieOut(ctx, pid, "2026-09", &payment.ReconciliationSummary{Period: "2026-09"}); err != nil {
		t.Fatalf("compute: %v", err)
	}
	if _, err := svc.CloseTieOut(ctx, pid, "2026-09"); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err := svc.ReopenTieOut(ctx, pid, "2026-09", "owner_123", "")
	if !errors.Is(err, ErrPeriodNotReopenable) {
		t.Fatalf("expected ErrPeriodNotReopenable for empty reason, got: %v", err)
	}
}

func TestReopenTieOut_Success(t *testing.T) {
	ctx := context.Background()
	pub := &capturingPublisher{}
	mem := NewMemoryStore()
	svc := NewService(mem, pub)
	svc.Now = func() time.Time { return time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC) }
	pid := uuid.New()

	if _, err := svc.ComputeTieOut(ctx, pid, "2026-09", &payment.ReconciliationSummary{Period: "2026-09"}); err != nil {
		t.Fatalf("compute: %v", err)
	}
	closed, err := svc.CloseTieOut(ctx, pid, "2026-09")
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	if closed.Status != "closed" {
		t.Fatalf("expected status closed, got %s", closed.Status)
	}

	reopened, err := svc.ReopenTieOut(ctx, pid, "2026-09", "owner-user-uuid", "Quarterly external auditor restatement")
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	if reopened.Status != "open" {
		t.Fatalf("expected status open, got: %s", reopened.Status)
	}

	stored, err := mem.GetTieOut(ctx, pid, "2026-09")
	if err != nil {
		t.Fatalf("get reopened tie-out: %v", err)
	}
	if stored.Status != "open" {
		t.Fatalf("expected status open after reopen, got: %s", stored.Status)
	}
	if stored.ClosedAt != nil {
		t.Fatalf("expected nil ClosedAt after reopen, got: %v", stored.ClosedAt)
	}

	// Verify event publication
	var reopenEvt *domain.Event
	for _, e := range pub.events {
		if e.EventType == domain.EvtTieOutReopened {
			reopenEvt = &e
			break
		}
	}
	if reopenEvt == nil {
		t.Fatalf("expected %s event to be published, none found", domain.EvtTieOutReopened)
	}
	if reopenEvt.PropertyID != pid {
		t.Fatalf("expected propertyID %v, got %v", pid, reopenEvt.PropertyID)
	}
}
