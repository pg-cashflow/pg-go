package finance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type noopPub struct{}

func (noopPub) Publish(context.Context, domain.Event) error { return nil }

func TestVoidExpense_Approved_NetsToZero(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	svc := NewService(store, noopPub{})

	propID := uuid.New()
	ownerID := uuid.New()

	// 1. Create an approved expense
	exp, _, err := svc.CreateExpense(ctx, CreateExpenseInput{
		PropertyID:     propID,
		ActorID:        ownerID,
		ActorRole:      "owner",
		CategoryCode:   "maintenance",
		VendorName:     "AC Repair",
		Description:    "Fixed AC unit",
		AmountPaise:    120000, // Rs 1,200
		IdempotencyKey: "exp-approved-1",
	})
	if err != nil {
		t.Fatalf("failed to create expense: %v", err)
	}
	if exp.Status != domain.ExpenseApproved {
		t.Fatalf("expected approved expense, got %s", exp.Status)
	}

	// Verify initial journal lines: OperatingExpense Dr 120000, AccountsPayable Cr 120000
	dr, cr, err := store.SumAccount(ctx, propID, domain.AcctOperatingExpense, time.Time{}, time.Time{})
	if err != nil || dr != 120000 || cr != 0 {
		t.Fatalf("expected operating expense dr=120000 cr=0, got dr=%d cr=%d (err=%v)", dr, cr, err)
	}

	// 2. Void the expense
	voided, err := svc.VoidExpense(ctx, VoidExpenseInput{
		PropertyID: propID,
		ExpenseID:  exp.ID,
		ActorID:    ownerID,
		ActorRole:  domain.PayerOwner,
		Reason:     "Mistyped amount, meant 12000",
	})
	if err != nil {
		t.Fatalf("failed to void expense: %v", err)
	}
	if voided.Status != domain.ExpenseCancelled {
		t.Fatalf("expected cancelled status, got %s", voided.Status)
	}
	if voided.VoidReason == nil || *voided.VoidReason != "Mistyped amount, meant 12000" {
		t.Fatalf("void reason not recorded properly")
	}

	// 3. Verify reversing journal entry posted: OperatingExpense Cr 120000, AccountsPayable Dr 120000
	dr, cr, err = store.SumAccount(ctx, propID, domain.AcctOperatingExpense, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("SumAccount error: %v", err)
	}
	if dr != 120000 || cr != 120000 {
		t.Fatalf("expected operating expense to net to zero (dr=120000 cr=120000), got dr=%d cr=%d", dr, cr)
	}

	drPayable, crPayable, err := store.SumAccount(ctx, propID, domain.AcctAccountsPayable, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("SumAccount error: %v", err)
	}
	if drPayable != 120000 || crPayable != 120000 {
		t.Fatalf("expected accounts payable to net to zero (dr=120000 cr=120000), got dr=%d cr=%d", drPayable, crPayable)
	}
}

func TestVoidExpense_PendingApproval_LeavesNoJournalAndCancelsApproval(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	svc := NewService(store, noopPub{})

	propID := uuid.New()
	managerID := uuid.New()
	ownerID := uuid.New()

	// Policy requires owner approval above Rs 5,000 (500000 paise)
	_ = store.SavePolicy(ctx, domain.ApprovalPolicy{
		PropertyID:                  propID,
		ManagerDailyLimitPaise:      1000000,
		SingleExpenseLimitPaise:     500000,
		ManagerMonthlyLimitPaise:    5000000,
		OwnerApprovalThresholdPaise: 500000,
	})

	// 1. Manager creates expense of Rs 6,000 -> pending approval
	exp, approval, err := svc.CreateExpense(ctx, CreateExpenseInput{
		PropertyID:     propID,
		ActorID:        managerID,
		ActorRole:      "manager",
		CategoryCode:   "furniture",
		VendorName:     "Beds Co",
		Description:    "New bunk bed",
		AmountPaise:    600000,
		IdempotencyKey: "exp-pending-1",
	})
	if err != nil {
		t.Fatalf("failed to create expense: %v", err)
	}
	if exp.Status != domain.ExpensePendingApproval {
		t.Fatalf("expected pending_approval, got %s", exp.Status)
	}
	if approval == nil || approval.Status != "pending" {
		t.Fatalf("expected pending approval request")
	}

	// Ensure no journal lines were posted
	lines, err := store.ListJournal(ctx, propID, time.Time{}, time.Time{}, "")
	if err != nil || len(lines) != 0 {
		t.Fatalf("expected 0 journal lines for pending expense, got %d", len(lines))
	}

	// 2. Void the pending expense
	voided, err := svc.VoidExpense(ctx, VoidExpenseInput{
		PropertyID: propID,
		ExpenseID:  exp.ID,
		ActorID:    managerID,
		ActorRole:  domain.PayerManager,
		Reason:     "Duplicate submission",
	})
	if err != nil {
		t.Fatalf("failed to void pending expense: %v", err)
	}
	if voided.Status != domain.ExpenseCancelled {
		t.Fatalf("expected cancelled status, got %s", voided.Status)
	}

	// Still 0 journal lines
	lines, _ = store.ListJournal(ctx, propID, time.Time{}, time.Time{}, "")
	if len(lines) != 0 {
		t.Fatalf("expected 0 journal lines after voiding pending expense, got %d", len(lines))
	}

	// 3. F1 Fix Verification: Owner tries to approve voided expense -> MUST FAIL
	err = svc.DecideApproval(ctx, propID, ownerID, approval.ID, true, "Approved bunk bed")
	if err == nil {
		t.Fatalf("expected error when approving voided expense, got nil")
	}
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden, got %v", err)
	}

	// Expense remains cancelled
	reloaded, _ := store.GetExpense(ctx, exp.ID)
	if reloaded.Status != domain.ExpenseCancelled {
		t.Fatalf("expected expense to remain cancelled, got %s", reloaded.Status)
	}
}

func TestVoidExpense_RejectsDoubleVoidAndPaid(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	svc := NewService(store, noopPub{})

	propID := uuid.New()
	ownerID := uuid.New()

	exp, _, err := svc.CreateExpense(ctx, CreateExpenseInput{
		PropertyID:     propID,
		ActorID:        ownerID,
		ActorRole:      "owner",
		CategoryCode:   "utilities",
		AmountPaise:    50000,
		IdempotencyKey: "exp-paid-test-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Pay the expense
	_, err = svc.PayExpense(ctx, PayExpenseInput{
		ExpenseID:      exp.ID,
		PropertyID:     propID,
		ActorID:        ownerID,
		ActorRole:      domain.PayerOwner,
		AmountPaise:    50000,
		Method:         "bank",
		IdempotencyKey: "pay-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Trying to void paid expense must fail
	_, err = svc.VoidExpense(ctx, VoidExpenseInput{
		PropertyID: propID,
		ExpenseID:  exp.ID,
		ActorID:    ownerID,
		ActorRole:  domain.PayerOwner,
		Reason:     "Mistake",
	})
	if err == nil {
		t.Fatalf("expected error voiding paid expense, got nil")
	}

	// Create another expense and void it
	exp2, _, err := svc.CreateExpense(ctx, CreateExpenseInput{
		PropertyID:     propID,
		ActorID:        ownerID,
		ActorRole:      "owner",
		CategoryCode:   "utilities",
		AmountPaise:    30000,
		IdempotencyKey: "exp-double-void-test",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = svc.VoidExpense(ctx, VoidExpenseInput{
		PropertyID: propID,
		ExpenseID:  exp2.ID,
		ActorID:    ownerID,
		ActorRole:  domain.PayerOwner,
		Reason:     "First void",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Second void must fail
	_, err = svc.VoidExpense(ctx, VoidExpenseInput{
		PropertyID: propID,
		ExpenseID:  exp2.ID,
		ActorID:    ownerID,
		ActorRole:  domain.PayerOwner,
		Reason:     "Second void attempt",
	})
	if err == nil {
		t.Fatalf("expected error on second void attempt, got nil")
	}
}

func TestVoidExpense_MakerCheckerThreshold(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	svc := NewService(store, noopPub{})

	propID := uuid.New()
	managerID := uuid.New()
	ownerID := uuid.New()

	// Owner approval threshold is Rs 5,000 (500000 paise)
	_ = store.SavePolicy(ctx, domain.ApprovalPolicy{
		PropertyID:                  propID,
		ManagerDailyLimitPaise:      10000000,
		SingleExpenseLimitPaise:     5000000,
		ManagerMonthlyLimitPaise:    50000000,
		OwnerApprovalThresholdPaise: 500000,
	})

	// Owner creates an approved expense of Rs 8,000
	exp, _, err := svc.CreateExpense(ctx, CreateExpenseInput{
		PropertyID:     propID,
		ActorID:        ownerID,
		ActorRole:      "owner",
		CategoryCode:   "repairs",
		AmountPaise:    800000,
		IdempotencyKey: "exp-maker-checker-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Manager attempts to void approved expense above threshold -> MUST FAIL
	_, err = svc.VoidExpense(ctx, VoidExpenseInput{
		PropertyID: propID,
		ExpenseID:  exp.ID,
		ActorID:    managerID,
		ActorRole:  domain.PayerManager,
		Reason:     "Manager void attempt",
	})
	if err == nil || !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden when manager attempts to void expense above threshold, got %v", err)
	}

	// Owner voids it -> SUCCESS
	voided, err := svc.VoidExpense(ctx, VoidExpenseInput{
		PropertyID: propID,
		ExpenseID:  exp.ID,
		ActorID:    ownerID,
		ActorRole:  domain.PayerOwner,
		Reason:     "Owner approved void",
	})
	if err != nil {
		t.Fatalf("expected owner void to succeed, got %v", err)
	}
	if voided.Status != domain.ExpenseCancelled {
		t.Fatalf("expected cancelled status, got %s", voided.Status)
	}
}

func TestVoidExpense_ManagerPermissions(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	svc := NewService(store, noopPub{})

	propID := uuid.New()
	mgr1ID := uuid.New()
	mgr2ID := uuid.New()

	// Threshold is Rs 100 (10000 paise) so Rs 500 requires approval
	_ = store.SavePolicy(ctx, domain.ApprovalPolicy{
		PropertyID:                  propID,
		ManagerDailyLimitPaise:      10000000,
		SingleExpenseLimitPaise:     5000000,
		ManagerMonthlyLimitPaise:    50000000,
		OwnerApprovalThresholdPaise: 10000,
	})

	// 1. Manager 1 creates a pending-approval expense
	exp1, _, err := svc.CreateExpense(ctx, CreateExpenseInput{
		PropertyID:     propID,
		ActorID:        mgr1ID,
		ActorRole:      "manager",
		CategoryCode:   "supplies",
		AmountPaise:    50000,
		IdempotencyKey: "mgr-pending-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if exp1.Status != domain.ExpensePendingApproval {
		t.Fatalf("expected pending_approval, got %s", exp1.Status)
	}

	// Manager 2 tries to void Manager 1's pending expense -> MUST FAIL
	_, err = svc.VoidExpense(ctx, VoidExpenseInput{
		PropertyID: propID,
		ExpenseID:  exp1.ID,
		ActorID:    mgr2ID,
		ActorRole:  domain.PayerManager,
		Reason:     "Mgr2 void attempt on Mgr1 expense",
	})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden when manager voids another manager's pending expense, got %v", err)
	}

	// Manager 1 voids own pending expense -> SUCCESS
	v1, err := svc.VoidExpense(ctx, VoidExpenseInput{
		PropertyID: propID,
		ExpenseID:  exp1.ID,
		ActorID:    mgr1ID,
		ActorRole:  domain.PayerManager,
		Reason:     "Mgr1 cancelled own pending expense",
	})
	if err != nil {
		t.Fatalf("expected mgr1 void own pending to succeed, got %v", err)
	}
	if v1.Status != domain.ExpenseCancelled {
		t.Fatalf("expected cancelled status, got %s", v1.Status)
	}
	if v1.VoidReason == nil || *v1.VoidReason != "Mgr1 cancelled own pending expense" {
		t.Fatalf("expected void_reason recorded on expense row")
	}

	// 2. Draft expense test
	draftExp := &domain.Expense{
		ID:             uuid.New(),
		PropertyID:     propID,
		CategoryCode:   "supplies",
		AmountPaise:    30000,
		Status:         domain.ExpenseDraft,
		CreatedBy:      mgr1ID,
		CreatedByRole:  "manager",
		IdempotencyKey: "mgr-draft-1",
	}
	store.expenses[draftExp.ID] = *draftExp

	// Manager 2 cannot void Manager 1's draft
	_, err = svc.VoidExpense(ctx, VoidExpenseInput{
		PropertyID: propID,
		ExpenseID:  draftExp.ID,
		ActorID:    mgr2ID,
		ActorRole:  domain.PayerManager,
		Reason:     "Mgr2 void attempt on draft",
	})
	if !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden when manager voids another manager's draft, got %v", err)
	}

	// Manager 1 voids own draft -> SUCCESS
	vDraft, err := svc.VoidExpense(ctx, VoidExpenseInput{
		PropertyID: propID,
		ExpenseID:  draftExp.ID,
		ActorID:    mgr1ID,
		ActorRole:  domain.PayerManager,
		Reason:     "Mgr1 cancelled draft",
	})
	if err != nil {
		t.Fatalf("expected mgr1 void draft to succeed, got %v", err)
	}
	if vDraft.Status != domain.ExpenseCancelled {
		t.Fatalf("expected cancelled status, got %s", vDraft.Status)
	}
}

func TestVoidExpense_RacesWithPay(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	svc := NewService(store, noopPub{})

	propID := uuid.New()
	ownerID := uuid.New()

	// 1. Create an approved expense
	exp, _, err := svc.CreateExpense(ctx, CreateExpenseInput{
		PropertyID:     propID,
		ActorID:        ownerID,
		ActorRole:      "owner",
		CategoryCode:   "repairs",
		AmountPaise:    100000,
		IdempotencyKey: "exp-race-pay-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 2. Simulate payment landing concurrently
	_, err = svc.PayExpense(ctx, PayExpenseInput{
		ExpenseID:      exp.ID,
		PropertyID:     propID,
		ActorID:        ownerID,
		ActorRole:      domain.PayerOwner,
		AmountPaise:    100000,
		Method:         "bank",
		IdempotencyKey: "pay-race-1",
	})
	if err != nil {
		t.Fatalf("failed to record payment: %v", err)
	}

	// 3. Now attempt to void the expense expecting status=approved -> MUST CONFLICT
	_, err = store.VoidExpenseAtomic(ctx, domain.VoidExpenseParams{
		PropertyID:     propID,
		ExpenseID:      exp.ID,
		ExpectedStatus: domain.ExpenseApproved,
		VoidedBy:       ownerID,
		VoidReason:     "Late void attempt racing with pay",
		VoidedAt:       time.Now().UTC(),
	})
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("expected ErrConflict when void races with payment, got %v", err)
	}

	// 4. Verify operating expense debit is NOT reversed (AP was settled by payment, not reversed twice)
	dr, cr, err := store.SumAccount(ctx, propID, domain.AcctOperatingExpense, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if dr != 100000 || cr != 0 {
		t.Fatalf("operating expense accounts payable reversed twice! expected dr=100000 cr=0, got dr=%d cr=%d", dr, cr)
	}
}

