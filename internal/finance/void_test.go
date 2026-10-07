package finance

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// voidFixture holds a service on the in-memory store and one property with an owner and a manager.
type voidFixture struct {
	svc   *Service
	st    *MemoryStore
	pid   uuid.UUID
	owner uuid.UUID
	mgr   uuid.UUID
}

func newVoidFixture() *voidFixture {
	st := NewMemoryStore()
	return &voidFixture{
		svc:   NewService(st, nil),
		st:    st,
		pid:   uuid.New(),
		owner: uuid.New(),
		mgr:   uuid.New(),
	}
}

// ownerExpense creates an owner expense. Owner expenses are approved at once.
func (f *voidFixture) ownerExpense(t *testing.T, key string, paise int64) *domain.Expense {
	t.Helper()
	e, _, err := f.svc.CreateExpense(context.Background(), CreateExpenseInput{
		PropertyID: f.pid, ActorID: f.owner, ActorRole: string(domain.RoleOwner),
		CategoryCode: "vendor", AmountPaise: paise, IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("create owner expense: %v", err)
	}
	if e.Status != domain.ExpenseApproved {
		t.Fatalf("owner expense status = %s, want approved", e.Status)
	}
	return e
}

// netByAccount returns debit minus credit per account for all journal lines of the property.
func (f *voidFixture) netByAccount() map[string]int64 {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	net := map[string]int64{}
	for _, l := range f.st.journal {
		if l.PropertyID == f.pid {
			net[l.AccountCode] += l.DebitPaise - l.CreditPaise
		}
	}
	return net
}

func (f *voidFixture) journalLen() int {
	f.st.mu.Lock()
	defer f.st.mu.Unlock()
	return len(f.st.journal)
}

func (f *voidFixture) void(e *domain.Expense, actor uuid.UUID, role domain.PayerRole, reason string) (*domain.Expense, error) {
	return f.svc.VoidExpense(context.Background(), VoidExpenseInput{
		PropertyID: f.pid, ExpenseID: e.ID, ActorID: actor, ActorRole: role, Reason: reason,
	})
}

func TestVoidApprovedExpenseNetsToZero(t *testing.T) {
	f := newVoidFixture()
	e := f.ownerExpense(t, "void-net-1", 12_345_00)

	got, err := f.void(e, f.owner, domain.PayerOwner, "typed wrong amount")
	if err != nil {
		t.Fatalf("void: %v", err)
	}
	if got.Status != domain.ExpenseCancelled {
		t.Fatalf("status = %s, want cancelled", got.Status)
	}
	net := f.netByAccount()
	if net[domain.AcctOperatingExpense] != 0 || net[domain.AcctAccountsPayable] != 0 {
		t.Fatalf("journal does not net to zero: %v", net)
	}
}

func TestVoidTwiceIsRejected(t *testing.T) {
	f := newVoidFixture()
	e := f.ownerExpense(t, "void-twice-1", 5_000_00)
	if _, err := f.void(e, f.owner, domain.PayerOwner, "first void"); err != nil {
		t.Fatalf("first void: %v", err)
	}
	before := f.journalLen()
	if _, err := f.void(e, f.owner, domain.PayerOwner, "second void"); !errors.Is(err, ErrExpenseNotVoidable) {
		t.Fatalf("second void err = %v, want ErrExpenseNotVoidable", err)
	}
	if f.journalLen() != before {
		t.Fatalf("second void changed the journal")
	}
}

func TestVoidReasonRequired(t *testing.T) {
	f := newVoidFixture()
	e := f.ownerExpense(t, "void-reason-1", 5_000_00)
	for _, reason := range []string{"", "   ", "ab"} {
		if _, err := f.void(e, f.owner, domain.PayerOwner, reason); !errors.Is(err, ErrReasonRequired) {
			t.Fatalf("reason %q: err = %v, want ErrReasonRequired", reason, err)
		}
	}
	long := make([]rune, 501)
	for i := range long {
		long[i] = 'x'
	}
	if _, err := f.void(e, f.owner, domain.PayerOwner, string(long)); !errors.Is(err, ErrReasonRequired) {
		t.Fatalf("501-character reason: err = %v, want ErrReasonRequired", err)
	}
}

func TestVoidPaidExpenseIsRejected(t *testing.T) {
	f := newVoidFixture()
	e := f.ownerExpense(t, "void-paid-1", 10_000_00)
	if _, err := f.svc.PayExpense(context.Background(), PayExpenseInput{
		ExpenseID: e.ID, PropertyID: f.pid, ActorID: f.owner, ActorRole: domain.PayerOwner,
		AmountPaise: 10_000_00, Method: "bank", IdempotencyKey: "void-paid-pay",
	}); err != nil {
		t.Fatalf("pay: %v", err)
	}
	if _, err := f.void(e, f.owner, domain.PayerOwner, "too late"); !errors.Is(err, ErrExpenseNotVoidable) {
		t.Fatalf("void paid err = %v, want ErrExpenseNotVoidable", err)
	}
}

// Defect F1: void then approve must not bring the expense back.
func TestVoidPendingExpenseCannotBeApprovedLater(t *testing.T) {
	f := newVoidFixture()
	ctx := context.Background()
	e, appr, err := f.svc.CreateExpense(ctx, CreateExpenseInput{
		PropertyID: f.pid, ActorID: f.mgr, ActorRole: string(domain.RoleManager),
		CategoryCode: "vendor", AmountPaise: 600_000, IdempotencyKey: "void-pending-1",
	})
	if err != nil {
		t.Fatalf("manager expense: %v", err)
	}
	if e.Status != domain.ExpensePendingApproval || appr == nil {
		t.Fatalf("want pending expense with approval, got status=%s approval=%v", e.Status, appr)
	}

	if _, err := f.void(e, f.mgr, domain.PayerManager, "duplicate entry"); err != nil {
		t.Fatalf("manager void own pending: %v", err)
	}
	if err := f.svc.DecideApproval(ctx, f.pid, f.owner, appr.ID, true, "ok"); err == nil {
		t.Fatalf("approval of a voided expense must fail")
	}
	got, err := f.st.GetExpense(ctx, e.ID)
	if err != nil {
		t.Fatalf("get expense: %v", err)
	}
	if got.Status != domain.ExpenseCancelled {
		t.Fatalf("status = %s, want cancelled", got.Status)
	}
	if n := f.journalLen(); n != 0 {
		t.Fatalf("journal has %d lines, want 0", n)
	}
}

// Defect F1 at store level: the approval decision must not flip a cancelled expense.
func TestDecideApprovalAtomicRejectsNonPendingExpense(t *testing.T) {
	f := newVoidFixture()
	ctx := context.Background()
	e, appr, err := f.svc.CreateExpense(ctx, CreateExpenseInput{
		PropertyID: f.pid, ActorID: f.mgr, ActorRole: string(domain.RoleManager),
		CategoryCode: "vendor", AmountPaise: 600_000, IdempotencyKey: "decide-guard-1",
	})
	if err != nil || appr == nil {
		t.Fatalf("setup: err=%v approval=%v", err, appr)
	}
	if err := f.st.UpdateExpenseStatus(ctx, e.ID, domain.ExpenseCancelled); err != nil {
		t.Fatalf("set cancelled: %v", err)
	}
	st := domain.ExpenseApproved
	now := time.Now().UTC()
	a := *appr
	a.Status = "approved"
	a.DecidedBy = &f.owner
	a.DecidedAt = &now
	if err := f.st.DecideApprovalAtomic(ctx, &a, &st, nil); !errors.Is(err, ErrExpenseStateChanged) {
		t.Fatalf("err = %v, want ErrExpenseStateChanged", err)
	}
}

// Defect F2: a stale status must be refused by the store, not applied.
func TestVoidExpenseAtomicRefusesStaleStatus(t *testing.T) {
	f := newVoidFixture()
	ctx := context.Background()
	e := f.ownerExpense(t, "void-stale-1", 5_000_00)
	if err := f.st.UpdateExpenseStatus(ctx, e.ID, domain.ExpensePaid); err != nil {
		t.Fatalf("set paid: %v", err)
	}
	err := f.st.VoidExpenseAtomic(ctx, e.ID, domain.ExpenseApproved, nil,
		domain.ExpenseVoid{Reason: "stale", VoidedBy: f.owner, VoidedAt: time.Now().UTC()})
	if !errors.Is(err, ErrExpenseStateChanged) {
		t.Fatalf("err = %v, want ErrExpenseStateChanged", err)
	}
	got, _ := f.st.GetExpense(ctx, e.ID)
	if got.Status != domain.ExpensePaid {
		t.Fatalf("status = %s, want paid (unchanged)", got.Status)
	}
}

func TestVoidMakerChecker(t *testing.T) {
	f := newVoidFixture()
	ctx := context.Background()

	// A manager cannot void an expense that the owner created.
	ownerExp := f.ownerExpense(t, "mc-owner-1", 1_000_00)
	if _, err := f.void(ownerExp, f.mgr, domain.PayerManager, "not mine"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("manager void of owner expense err = %v, want ErrForbidden", err)
	}

	// A manager expense above the owner threshold, approved by emergency bypass:
	// the manager cannot void it, the owner can.
	big, _, err := f.svc.CreateExpense(ctx, CreateExpenseInput{
		PropertyID: f.pid, ActorID: f.mgr, ActorRole: string(domain.RoleManager),
		CategoryCode: "vendor", AmountPaise: 600_000, Emergency: true, IdempotencyKey: "mc-big-1",
	})
	if err != nil {
		t.Fatalf("emergency expense: %v", err)
	}
	if big.Status != domain.ExpenseApproved {
		t.Fatalf("emergency expense status = %s, want approved", big.Status)
	}
	if _, err := f.void(big, f.mgr, domain.PayerManager, "my mistake"); !errors.Is(err, ErrApprovalRequired) {
		t.Fatalf("manager void above threshold err = %v, want ErrApprovalRequired", err)
	}
	if _, err := f.void(big, f.owner, domain.PayerOwner, "owner correction"); err != nil {
		t.Fatalf("owner void: %v", err)
	}
}

func TestVoidConcurrentExactlyOneWins(t *testing.T) {
	f := newVoidFixture()
	e := f.ownerExpense(t, "void-conc-1", 7_000_00)

	const workers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, notVoidable, other := 0, 0, 0
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			_, err := f.void(e, f.owner, domain.PayerOwner, "concurrent void")
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, ErrExpenseNotVoidable):
				notVoidable++
			default:
				other++
			}
		}()
	}
	wg.Wait()

	if ok != 1 || other != 0 || notVoidable != workers-1 {
		t.Fatalf("ok=%d notVoidable=%d other=%d", ok, notVoidable, other)
	}
	net := f.netByAccount()
	if net[domain.AcctOperatingExpense] != 0 || net[domain.AcctAccountsPayable] != 0 {
		t.Fatalf("journal does not net to zero: %v", net)
	}
}

func TestValidateExpenseDate(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		at   time.Time
		want error
	}{
		{"now", now, nil},
		{"yesterday", now.Add(-24 * time.Hour), nil},
		{"89 days back", now.Add(-89 * 24 * time.Hour), nil},
		{"91 days back", now.Add(-91 * 24 * time.Hour), ErrDateOutOfRange},
		{"1 minute ahead (clock skew)", now.Add(time.Minute), nil},
		{"1 day ahead", now.Add(24 * time.Hour), ErrDateOutOfRange},
	}
	for _, c := range cases {
		if got := ValidateExpenseDate(now, c.at); !errors.Is(got, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
