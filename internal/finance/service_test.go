package finance

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

func TestJournalBalanceAndManagerAdvance(t *testing.T) {
	st := NewMemoryStore()
	svc := NewService(st, nil)
	ctx := context.Background()
	pid := uuid.New()
	owner := uuid.New()
	mgr := uuid.New()

	if _, err := svc.AddCapital(ctx, pid, owner, domain.CapitalInitial, 100_000_00, "setup", "k-cap-1"); err != nil {
		t.Fatalf("capital: %v", err)
	}
	if _, err := svc.AddCapital(ctx, pid, owner, domain.CapitalInitial, 1, "dup", "k-cap-1"); err != ErrDuplicateIdempotency {
		t.Fatalf("expected dup, got %v", err)
	}

	e, _, err := svc.CreateExpense(ctx, CreateExpenseInput{
		PropertyID: pid, ActorID: owner, ActorRole: string(domain.RoleOwner),
		CategoryCode: "vendor", AmountPaise: 10_000_00, IdempotencyKey: "exp-1",
	})
	if err != nil {
		t.Fatalf("expense: %v", err)
	}
	if _, err := svc.PayExpense(ctx, PayExpenseInput{
		ExpenseID: e.ID, PropertyID: pid, ActorID: mgr, ActorRole: domain.PayerManager,
		AmountPaise: 4_000_00, Method: "cash", IdempotencyKey: "pay-m",
	}); err != nil {
		t.Fatalf("mgr pay: %v", err)
	}
	if _, err := svc.PayExpense(ctx, PayExpenseInput{
		ExpenseID: e.ID, PropertyID: pid, ActorID: owner, ActorRole: domain.PayerOwner,
		AmountPaise: 6_000_00, Method: "bank", IdempotencyKey: "pay-o",
	}); err != nil {
		t.Fatalf("owner pay: %v", err)
	}
	out, err := st.AdvanceOutstanding(ctx, pid)
	if err != nil || out != 4_000_00 {
		t.Fatalf("outstanding=%d err=%v", out, err)
	}
	if _, err := svc.ReimburseManager(ctx, pid, owner, mgr, 4_000_00, "reimb-1"); err != nil {
		t.Fatalf("reimburse: %v", err)
	}
	out, _ = st.AdvanceOutstanding(ctx, pid)
	if out != 0 {
		t.Fatalf("after reimburse outstanding=%d", out)
	}

	lines, _ := st.ListJournal(ctx, pid, time.Time{}, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), "")
	var d, c int64
	for _, l := range lines {
		d += l.DebitPaise
		c += l.CreditPaise
	}
	if d != c {
		t.Fatalf("unbalanced journal debit=%d credit=%d n=%d", d, c, len(lines))
	}
}

func TestTieOutBlocksUnexplained(t *testing.T) {
	st := NewMemoryStore()
	svc := NewService(st, nil)
	ctx := context.Background()
	pid := uuid.New()
	recon := &payment.ReconciliationSummary{RentCollected: 1000, Period: "2026-09"}
	t1, err := svc.ComputeTieOut(ctx, pid, "2026-09", recon)
	if err != nil {
		t.Fatalf("tie-out: %v", err)
	}
	if t1.DifferencePaise != 1000 {
		t.Fatalf("diff=%d", t1.DifferencePaise)
	}
	if _, err := svc.CloseTieOut(ctx, pid, "2026-09"); err != ErrPeriodNotCloseable {
		t.Fatalf("expected block, got %v", err)
	}
}

func TestVarianceBridgeTDREstimated(t *testing.T) {
	br := BuildVarianceBridge(VarianceInput{
		Period:             "2026-09",
		BudgetedOCFPaise:   120_000_00,
		CollectedRentPaise: 100_000_00,
		BilledRentPaise:    106_200_00,
		TDRPaise:           800_00,
		TDREstimated:       true,
		LoyaltyVariance:    3_100_00,
	}, 79_700_00)
	found := false
	for _, l := range br.Lines {
		if l.Driver == "payment_processing_tdr" {
			found = true
			if l.Kind != "estimated" {
				t.Fatalf("TDR kind=%s", l.Kind)
			}
		}
	}
	if !found {
		t.Fatal("missing TDR line")
	}
}

func TestPolicyDBCheckNotJWT(t *testing.T) {
	p := defaultPolicy(uuid.New())
	chk := evaluateManagerSpend(p, p.OwnerApprovalThresholdPaise+1, 0, 0, false)
	if !chk.NeedsApproval {
		t.Fatal("expected approval")
	}
	chk = evaluateManagerSpend(p, p.ManagerDailyLimitPaise+1, 0, 0, false)
	if chk.Reject != ErrPolicyExceeded {
		t.Fatalf("daily: %+v", chk)
	}
}

type capturePublisher struct {
	events []domain.Event
}

func (p *capturePublisher) Publish(ctx context.Context, e domain.Event) error {
	p.events = append(p.events, e)
	return nil
}

func TestPatchUnifiedSettings(t *testing.T) {
	st := NewMemoryStore()
	pub := &capturePublisher{}
	svc := NewService(st, pub)
	ctx := context.Background()
	pid := uuid.New()

	stIn := &domain.PropertyFinanceSettings{
		FiscalMonthStartDay: 5,
		ManagerCanViewROI:   true,
		TDREffectiveBPS:     180,
		TDRIsEstimated:      false,
	}
	polIn := &domain.ApprovalPolicy{
		ManagerDailyLimitPaise: 2000000,
	}
	loyIn := &domain.PropertyGamificationSettings{
		PointValuePaise:    150,
		MonthlyBudgetPaise: 500000,
	}

	stOut, polOut, loyOut, err := svc.PatchUnifiedSettings(ctx, pid, stIn, polIn, loyIn)
	if err != nil {
		t.Fatalf("patch settings failed: %v", err)
	}
	if stOut.FiscalMonthStartDay != 5 || !stOut.ManagerCanViewROI {
		t.Fatalf("settings not applied: %+v", stOut)
	}
	if polOut.ManagerDailyLimitPaise != 2000000 {
		t.Fatalf("policy not applied: %+v", polOut)
	}
	if loyOut == nil || loyOut.PointValuePaise != 150 {
		t.Fatalf("loyalty not applied: %+v", loyOut)
	}

	foundEvt := false
	for _, e := range pub.events {
		if e.EventType == domain.EvtFinancePolicyChanged {
			foundEvt = true
		}
	}
	if !foundEvt {
		t.Fatal("EvtFinancePolicyChanged not published")
	}
}

func TestRecurringTieOutAging(t *testing.T) {
	st := NewMemoryStore()
	pub := &capturePublisher{}
	svc := NewService(st, pub)
	ctx := context.Background()
	pid := uuid.New()

	// Month 1
	recon1 := &payment.ReconciliationSummary{RentCollected: 1000, Period: "2026-07"}
	t1, err := svc.ComputeTieOut(ctx, pid, "2026-07", recon1)
	if err != nil || t1.DifferencePaise != 1000 {
		t.Fatalf("m1: %+v err: %v", t1, err)
	}

	// Month 2 - reaching 2 consecutive unresolved periods now triggers EvtRecurringTieOutException
	recon2 := &payment.ReconciliationSummary{RentCollected: 1000, Period: "2026-08"}
	t2, err := svc.ComputeTieOut(ctx, pid, "2026-08", recon2)
	if err != nil || t2.DifferencePaise != 1000 {
		t.Fatalf("m2: %+v err: %v", t2, err)
	}

	foundAging := false
	for _, e := range pub.events {
		if e.EventType == domain.EvtRecurringTieOutException {
			foundAging = true
		}
	}
	if !foundAging {
		t.Fatalf("expected EvtRecurringTieOutException on month 2 aging, got: %+v (items: %+v)", pub.events, t2.Items)
	}
}

func TestCriticalTieOutVarianceAlert(t *testing.T) {
	st := NewMemoryStore()
	pub := &capturePublisher{}
	svc := NewService(st, pub)
	ctx := context.Background()
	pid := uuid.New()

	// Single period variance of ₹6,000 (600,000 paise) >= ₹5,000 threshold
	recon := &payment.ReconciliationSummary{RentCollected: 600000, Period: "2026-07"}
	t1, err := svc.ComputeTieOut(ctx, pid, "2026-07", recon)
	if err != nil || t1.DifferencePaise != 600000 {
		t.Fatalf("m1: %+v err: %v", t1, err)
	}

	foundCritical := false
	for _, e := range pub.events {
		if e.EventType == domain.EvtCriticalTieOutVariance {
			foundCritical = true
		}
	}
	if !foundCritical {
		t.Fatalf("expected EvtCriticalTieOutVariance for single-month variance exceeding ₹5,000 threshold, got: %+v", pub.events)
	}
}

func TestIdempotencyDuplicates(t *testing.T) {
	st := NewMemoryStore()
	svc := NewService(st, nil)
	ctx := context.Background()
	pid := uuid.New()
	owner := uuid.New()

	_, _, err := svc.CreateExpense(ctx, CreateExpenseInput{
		PropertyID: pid, ActorID: owner, ActorRole: string(domain.RoleOwner),
		CategoryCode: "repairs", AmountPaise: 500000, IdempotencyKey: "dup-exp-test",
	})
	if err != nil {
		t.Fatalf("first expense failed: %v", err)
	}

	_, _, err = svc.CreateExpense(ctx, CreateExpenseInput{
		PropertyID: pid, ActorID: owner, ActorRole: string(domain.RoleOwner),
		CategoryCode: "repairs", AmountPaise: 500000, IdempotencyKey: "dup-exp-test",
	})
	if err != ErrDuplicateIdempotency {
		t.Fatalf("expected ErrDuplicateIdempotency on duplicate key, got: %v", err)
	}
}

func TestMirrorDepartureSettlement_WithPriorOverdueDues_Balances(t *testing.T) {
	st := NewMemoryStore()
	svc := NewService(st, nil)
	ctx := context.Background()
	pid := uuid.New()
	depID := uuid.New()

	// Case 1: Deposit covers overdue dues + deductions, remainder refunded to tenant.
	// Deposit: 10,000 INR (1,000,000 paise)
	// Deductions/damages: 1,000 INR (100,000 paise)
	// Overdue dues netted: 3,000 INR (300,000 paise)
	// Net refund: 6,000 INR (600,000 paise)
	err := svc.MirrorDepartureSettlement(ctx, pid, depID, 1_000_000, 0, 100_000, 600_000, 300_000, 0, time.Now())
	if err != nil {
		t.Fatalf("MirrorDepartureSettlement failed: %v", err)
	}

	lines, err := st.ListJournal(ctx, pid, time.Time{}, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), "")
	if err != nil {
		t.Fatalf("ListJournal failed: %v", err)
	}
	var debits, credits int64
	for _, l := range lines {
		debits += l.DebitPaise
		credits += l.CreditPaise
	}
	if debits != credits || debits != 1_000_000 {
		t.Fatalf("journal imbalance: debits=%d credits=%d", debits, credits)
	}

	// Case 2: Overdue dues + damages exceed deposit, leaving a receivable balance.
	// Deposit: 1,000,000 paise
	// Deductions: 200,000 paise
	// Overdue dues netted: 1,200,000 paise
	// Net refund: 0 paise
	// Receivable balance: 400,000 paise
	depID2 := uuid.New()
	err = svc.MirrorDepartureSettlement(ctx, pid, depID2, 1_000_000, 0, 200_000, 0, 1_200_000, 400_000, time.Now())
	if err != nil {
		t.Fatalf("MirrorDepartureSettlement with receivable failed: %v", err)
	}

	lines2, err := st.ListJournal(ctx, pid, time.Time{}, time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), "")
	if err != nil {
		t.Fatalf("ListJournal failed: %v", err)
	}
	var totalD, totalC int64
	for _, l := range lines2 {
		totalD += l.DebitPaise
		totalC += l.CreditPaise
	}
	if totalD != totalC {
		t.Fatalf("cumulative journal imbalance: totalD=%d totalC=%d", totalD, totalC)
	}
}

