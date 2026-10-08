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

// TestLedger_RedeemApplyCreditPayRest_TrialBalanceZero tests the complete lifecycle:
// 1. Issue loyalty points to tenant
// 2. Tenant redeems points for rent credit (credits tenant_receivable, debits reward_liability)
// 3. Apply credit to rent due (debits tenant_receivable, credits rent_revenue)
// 4. Tenant pays remaining rent due via bank/cash (debits bank, credits rent_revenue)
// 5. Assert trial balance is balanced, tenant_receivable is zero, and full revenue is recognized.
func TestLedger_RedeemApplyCreditPayRest_TrialBalanceZero(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	svc := NewService(st, nil)

	pid := uuid.New()
	tid := uuid.New()
	did := uuid.New()
	redID := uuid.New()
	payID := uuid.New()
	ledgerID := uuid.New()
	now := time.Now().UTC()

	// 1. Points issued: 50 points worth 5000 paise
	const creditPaise int64 = 5000
	const remainingRentPaise int64 = 15000
	const totalRentPaise int64 = creditPaise + remainingRentPaise // 20000

	if err := svc.MirrorPointsIssued(ctx, pid, tid, ledgerID, 50, creditPaise); err != nil {
		t.Fatalf("MirrorPointsIssued failed: %v", err)
	}

	// 2. Redeem reward for rent credit: 50 points spent, 5000 paise credit
	if err := svc.MirrorRewardRedeem(ctx, pid, tid, redID, 50, creditPaise); err != nil {
		t.Fatalf("MirrorRewardRedeem failed: %v", err)
	}

	// 3. Apply credit against rent due
	if err := svc.MirrorApplyCredit(ctx, pid, did, creditPaise, domain.DueKindRent, now); err != nil {
		t.Fatalf("MirrorApplyCredit failed: %v", err)
	}

	// 4. Pay the remaining due amount (15000 paise) via bank (manual match routes to AcctBank)
	payment := &domain.Payment{
		ID:        payID,
		Amount:    remainingRentPaise,
		MatchedBy: domain.MatchedByManual,
		MatchedAt: now,
	}
	due := &domain.Due{
		ID:         did,
		PropertyID: pid,
		Kind:       domain.DueKindRent,
	}
	if err := svc.MirrorPayment(ctx, payment, due); err != nil {
		t.Fatalf("MirrorPayment failed: %v", err)
	}

	// 5. Query Trial Balance
	tb, err := svc.TrialBalance(ctx, pid, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("TrialBalance failed: %v", err)
	}

	var totalDebit, totalCredit int64
	tbMap := make(map[string]domain.TrialBalanceLine)
	for _, row := range tb {
		totalDebit += row.DebitPaise
		totalCredit += row.CreditPaise
		tbMap[row.AccountCode] = row
	}

	if totalDebit != totalCredit {
		t.Fatalf("trial balance not balanced: totalDebit=%d totalCredit=%d diff=%d",
			totalDebit, totalCredit, totalDebit-totalCredit)
	}

	// Invariant: tenant_receivable balance must be exactly zero
	receivableRow, ok := tbMap[domain.AcctTenantReceivable]
	if ok && receivableRow.BalancePaise != 0 {
		t.Fatalf("expected tenant_receivable balance to be 0, got %d (debit=%d credit=%d)",
			receivableRow.BalancePaise, receivableRow.DebitPaise, receivableRow.CreditPaise)
	}
	if !ok {
		// If not present in map, then debit == credit == 0 which is also zero balance.
	} else if receivableRow.DebitPaise != creditPaise || receivableRow.CreditPaise != creditPaise {
		t.Fatalf("expected tenant_receivable debit=%d and credit=%d, got debit=%d credit=%d",
			creditPaise, creditPaise, receivableRow.DebitPaise, receivableRow.CreditPaise)
	}

	// Invariant: full rent revenue must be recognized (20000 paise = credit 5000 + bank 15000)
	revRow, ok := tbMap[domain.AcctRentRevenue]
	if !ok {
		t.Fatalf("expected rent_revenue in trial balance, but not found")
	}
	if revRow.CreditPaise != totalRentPaise {
		t.Fatalf("expected rent_revenue credit to be %d, got %d", totalRentPaise, revRow.CreditPaise)
	}
}
