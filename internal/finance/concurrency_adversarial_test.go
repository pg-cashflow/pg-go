package finance

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// TestConcurrent_ExpensePayments_NoOverpayment verifies REQ-EXP-002 (P0-04):
// 50 concurrent payment attempts against a single expense cannot cause cumulative payments
// to exceed the total approved expense amount.
func TestConcurrent_ExpensePayments_NoOverpayment(t *testing.T) {
	st := NewMemoryStore()
	svc := NewService(st, nil)
	ctx := context.Background()

	pid := uuid.New()
	ownerID := uuid.New()
	totalExpensePaise := int64(100_000_00) // ₹100,000

	// 1. Create an approved expense
	e, _, err := svc.CreateExpense(ctx, CreateExpenseInput{
		PropertyID:     pid,
		ActorID:        ownerID,
		ActorRole:      string(domain.RoleOwner),
		CategoryCode:   "maintenance",
		AmountPaise:    totalExpensePaise,
		IdempotencyKey: "exp-conc-1",
	})
	if err != nil {
		t.Fatalf("create expense: %v", err)
	}

	// 2. Launch 50 concurrent payment requests of ₹10,000 each (Total attempted = ₹500,000)
	const concurrency = 50
	paymentPaise := int64(10_000_00) // ₹10,000

	var successCount int64
	var overpayCount int64
	var otherErrors int64

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start // synchronize launch

			idem := fmt.Sprintf("pay-conc-%d", idx)
			_, pErr := svc.PayExpense(ctx, PayExpenseInput{
				ExpenseID:      e.ID,
				PropertyID:     pid,
				ActorID:        ownerID,
				ActorRole:      domain.PayerOwner,
				AmountPaise:    paymentPaise,
				Method:         "bank",
				IdempotencyKey: idem,
			})
			if pErr == nil {
				atomic.AddInt64(&successCount, 1)
			} else if errors.Is(pErr, ErrOverpay) {
				atomic.AddInt64(&overpayCount, 1)
			} else {
				atomic.AddInt64(&otherErrors, 1)
			}
		}(i)
	}

	close(start) // trigger simultaneous execution
	wg.Wait()

	// 3. Invariants verification:
	// Exactly 10 payments of ₹10,000 can fit into ₹100,000.
	// Remaining 40 must be rejected with ErrOverpay.
	if successCount != 10 {
		t.Errorf("expected exactly 10 successful payments, got %d", successCount)
	}
	if overpayCount != 40 {
		t.Errorf("expected exactly 40 overpayment rejections, got %d", overpayCount)
	}
	if otherErrors != 0 {
		t.Errorf("expected 0 unexpected errors, got %d", otherErrors)
	}

	// Final sum in database must be strictly equal to the expense amount
	sumPaid, err := st.SumExpensePayments(ctx, e.ID)
	if err != nil {
		t.Fatalf("sum payments: %v", err)
	}
	if sumPaid != totalExpensePaise {
		t.Fatalf("invariant broken: sum paid %d != total expense %d", sumPaid, totalExpensePaise)
	}
}

// TestConcurrent_ManagerSpendLimits_Enforcement verifies REQ-EXP-003 (P0-05):
// 50 concurrent manager expense requests around the spend threshold commit only up to the limit
// and reject or route surplus to approval.
func TestConcurrent_ManagerSpendLimits_Enforcement(t *testing.T) {
	st := NewMemoryStore()
	svc := NewService(st, nil)
	ctx := context.Background()

	pid := uuid.New()
	managerID := uuid.New()

	// Configure policy: Daily limit = ₹50,000, Auto-approval cap = ₹20,000
	policy := domain.ApprovalPolicy{
		PropertyID:                  pid,
		ManagerDailyLimitPaise:      50_000_00,
		ManagerMonthlyLimitPaise:    500_000_00,
		SingleExpenseLimitPaise:     20_000_00,
		OwnerApprovalThresholdPaise: 20_000_00,
		EmergencyBypassEnabled:      false,
	}
	if err := st.SavePolicy(ctx, policy); err != nil {
		t.Fatalf("save policy: %v", err)
	}

	// Initial spend = ₹40,000
	now := time.Now().UTC()
	st.SaveInitialManagerSpend(pid, managerID, 40_000_00, now)

	// Launch 50 concurrent manager submissions of ₹10,000 each
	const concurrency = 50
	expenseAmount := int64(10_000_00) // ₹10,000

	var approvedCount int64
	var pendingOrRejectedCount int64

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start

			idem := fmt.Sprintf("mgr-conc-exp-%d", idx)
			e, _, err := svc.CreateExpense(ctx, CreateExpenseInput{
				PropertyID:     pid,
				ActorID:        managerID,
				ActorRole:      string(domain.RoleManager),
				CategoryCode:   "repairs",
				AmountPaise:    expenseAmount,
				IdempotencyKey: idem,
				OccurredAt:     now,
			})
			if idx == 0 {
				if err != nil {
					t.Logf("idx 0 error: %v", err)
				} else {
					t.Logf("idx 0 status: %v", e.Status)
				}
			}
			if err == nil {
				if e.Status == domain.ExpenseApproved {
					atomic.AddInt64(&approvedCount, 1)
				} else {
					atomic.AddInt64(&pendingOrRejectedCount, 1)
				}
			} else {
				atomic.AddInt64(&pendingOrRejectedCount, 1)
			}
		}(i)
	}

	close(start)
	wg.Wait()

	t.Logf("approvedCount: %d, pendingOrRejectedCount: %d", approvedCount, pendingOrRejectedCount)
	if approvedCount > 1 {
		t.Fatalf("concurrency limit breached: %d expenses approved, maximum allowed was 1", approvedCount)
	}
}

// TestConcurrent_TenantCredit_AtomicUpdates verifies REQ-TEN-001 (P0-06):
// 50 concurrent credit operations result in the exact mathematical sum without lost updates.
func TestConcurrent_TenantCredit_AtomicUpdates(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()

	tenantID := uuid.New()
	const concurrency = 50
	const deltaPaise = int64(500_00) // ₹500

	var wg sync.WaitGroup
	start := make(chan struct{})

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = st.AddTenantCredit(ctx, tenantID, deltaPaise)
		}()
	}

	close(start)
	wg.Wait()

	expectedBalance := int64(concurrency) * deltaPaise // 50 * 500 = ₹25,000
	actualBalance := st.GetTenantCredit(tenantID)
	if actualBalance != expectedBalance {
		t.Fatalf("lost update under concurrency: expected %d paise, got %d paise", expectedBalance, actualBalance)
	}
}
