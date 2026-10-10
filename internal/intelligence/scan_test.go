package intelligence

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

func TestScanVacancyAndLoyalty(t *testing.T) {
	st := finance.NewMemoryStore()
	svc := NewService(st)
	pid := uuid.New()
	leaks, _, err := svc.Scan(context.Background(), ScanInput{
		PropertyID:         pid,
		Recon:              &payment.ReconciliationSummary{OutstandingRent: 620000},
		Occupancy:          finance.Occupancy{CapacityBeds: 10, OccupiedBeds: 6, BedsAtRisk: 1},
		LoyaltyIssuedPaise: 200000,
		LoyaltyBudgetPaise: 100000,
		MealExpected:       map[string]int{"lunch": 70},
		MealPrepared:       map[string]int{"lunch": 85},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(leaks) < 3 {
		t.Fatalf("leaks=%d", len(leaks))
	}
}

func TestCOI(t *testing.T) {
	if COI(42800, 6) != 256800 {
		t.Fatalf("coi=%d", COI(42800, 6))
	}
}
