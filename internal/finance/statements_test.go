package finance

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockDeadLetterNotifier struct {
	events []*domain.LedgerOutboxEvent
	errors []string
}

func (m *mockDeadLetterNotifier) NotifyDeadLetter(_ context.Context, evt *domain.LedgerOutboxEvent, failureErr string) error {
	m.events = append(m.events, evt)
	m.errors = append(m.errors, failureErr)
	return nil
}

func TestStatements_Reports(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryStore()
	svc := NewService(mem, nil)
	pid := uuid.New()
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	// Post some journal lines in memory
	_ = mem.InsertJournal(ctx, []domain.JournalLine{
		{
			ID:          uuid.New(),
			PropertyID:  pid,
			OccurredAt:  from.Add(24 * time.Hour),
			AccountCode: domain.AcctRentRevenue,
			CreditPaise: 5000000,
		},
		{
			ID:          uuid.New(),
			PropertyID:  pid,
			OccurredAt:  from.Add(24 * time.Hour),
			AccountCode: domain.AcctOperatingExpense,
			DebitPaise:  1500000,
		},
	})

	// Income statement
	inc, err := svc.IncomeStatement(ctx, pid, from, to)
	if err != nil {
		t.Fatalf("income statement failed: %v", err)
	}
	if len(inc) != 3 {
		t.Fatalf("expected 3 income statement lines, got %d", len(inc))
	}
	// Check net income = 5000000 - 1500000 = 3500000
	for _, l := range inc {
		if l.Section == "net_income" && l.AmountPaise != 3500000 {
			t.Fatalf("expected net_income 3500000, got %d", l.AmountPaise)
		}
	}

	// Balance sheet
	bs, err := svc.BalanceSheet(ctx, pid, to)
	if err != nil {
		t.Fatalf("balance sheet failed: %v", err)
	}
	if len(bs) == 0 {
		t.Fatalf("expected balance sheet lines, got empty")
	}

	// Cash flow
	cf, err := svc.CashFlow(ctx, pid, from, to)
	if err != nil {
		t.Fatalf("cash flow failed: %v", err)
	}
	if len(cf) == 0 {
		t.Fatalf("expected cash flow lines, got empty")
	}

	// Trial balance
	tb, err := svc.TrialBalance(ctx, pid, to)
	if err != nil {
		t.Fatalf("trial balance failed: %v", err)
	}
	if len(tb) != 2 {
		t.Fatalf("expected 2 trial balance lines, got %d", len(tb))
	}

	// Reconciling items
	items, err := svc.ReconcilingItems(ctx, pid, to)
	if err != nil {
		t.Fatalf("reconciling items failed: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 items initially, got %d", len(items))
	}
}

type customReconcilingStore struct {
	*MemoryStore
	items []domain.ReconcilingItem
}

func (c *customReconcilingStore) GetReconcilingItems(_ context.Context, _ *uuid.UUID, _ time.Time) ([]domain.ReconcilingItem, error) {
	return c.items, nil
}

func TestScanAndAlertReconcilingItems(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryStore()
	pub := &capturingPublisher{}
	notifier := &mockDeadLetterNotifier{}

	amt1 := int64(1000)
	amt2 := int64(50000)

	cstore := &customReconcilingStore{
		MemoryStore: mem,
		items: []domain.ReconcilingItem{
			{
				ItemType:     "transient_sync",
				Category:     "control_hygiene",
				Escalation:   "none",
				AmountPaise:  &amt1,
				OriginatedOn: "2026-10-01",
				AgeDays:      2,
				AgeBucket:    "0-7d",
			},
			{
				ItemType:     "payment_unposted",
				Category:     "money_integrity",
				Escalation:   "owner",
				AmountPaise:  &amt2,
				OriginatedOn: "2026-09-25",
				AgeDays:      9,
				AgeBucket:    "8-30d",
			},
		},
	}

	svc := NewService(cstore, pub)
	pid := uuid.New()
	asOf := time.Now()

	items, err := svc.ScanAndAlertReconcilingItems(ctx, &pid, asOf, notifier)
	if err != nil {
		t.Fatalf("ScanAndAlertReconcilingItems failed: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 items scanned, got %d", len(items))
	}

	// Verify dead letter notifier received only the critical money integrity alert
	if len(notifier.events) != 1 {
		t.Fatalf("expected exactly 1 critical alert in dead letter, got %d", len(notifier.events))
	}
	if notifier.events[0].EventType != "reconciling_anomaly:payment_unposted" {
		t.Fatalf("expected EventType reconciling_anomaly:payment_unposted, got %s", notifier.events[0].EventType)
	}

	// Verify published event
	var foundEvt bool
	for _, e := range pub.events {
		if e.EventType == domain.EvtReconcilingAlert {
			foundEvt = true
			break
		}
	}
	if !foundEvt {
		t.Fatalf("expected event %s to be published", domain.EvtReconcilingAlert)
	}
}
