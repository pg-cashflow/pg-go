package finance

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockPropsLister struct {
	props []domain.Property
}

func (m *mockPropsLister) List(_ context.Context) ([]domain.Property, error) {
	return m.props, nil
}

func TestRecurringExpenseScheduler(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryStore()
	svc := NewService(mem, nil)

	pid := uuid.New()
	ownerID := uuid.New()
	mem.SetPropertyOwnerUserID(pid, ownerID)

	asOf := time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC)
	period := "2026-09"

	// Set a monthly budget for internet and lease
	_ = mem.UpsertBudget(ctx, &domain.Budget{
		PropertyID:   pid,
		CategoryCode: "internet",
		PeriodMonth:  period,
		AmountPaise:  500000,
	})
	_ = mem.UpsertBudget(ctx, &domain.Budget{
		PropertyID:   pid,
		CategoryCode: "lease",
		PeriodMonth:  period,
		AmountPaise:  50000000,
	})

	props := &mockPropsLister{
		props: []domain.Property{
			{
				ID:   pid,
				Name: "Test PG",
			},
		},
	}

	scheduler := NewRecurringExpenseScheduler(svc, mem, props)

	// First execution: should accrue expenses
	err := scheduler.ProcessRecurringExpenses(ctx, asOf)
	if err != nil {
		t.Fatalf("ProcessRecurringExpenses failed: %v", err)
	}

	expenses, err := mem.ListExpenses(ctx, pid)
	if err != nil {
		t.Fatalf("list expenses: %v", err)
	}
	if len(expenses) != 2 {
		t.Fatalf("expected 2 accrued expenses, got %d", len(expenses))
	}
	for _, e := range expenses {
		if e.Status != domain.ExpenseApproved {
			t.Fatalf("expected recurring expense to be auto-approved, got %s", e.Status)
		}
		if e.CreatedBy != ownerID {
			t.Fatalf("expected created by %v, got %v", ownerID, e.CreatedBy)
		}
	}

	// Second execution: must be idempotent and succeed without duplicate rows or errors
	err = scheduler.ProcessRecurringExpenses(ctx, asOf)
	if err != nil {
		t.Fatalf("second ProcessRecurringExpenses must succeed idempotently, got: %v", err)
	}

	expensesAfter, err := mem.ListExpenses(ctx, pid)
	if err != nil {
		t.Fatalf("list expenses after second run: %v", err)
	}
	if len(expensesAfter) != 2 {
		t.Fatalf("expected still 2 expenses after second run, got %d", len(expensesAfter))
	}
}
