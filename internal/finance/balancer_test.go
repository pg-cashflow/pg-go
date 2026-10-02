package finance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockBalancerStore struct {
	items map[string]*domain.DailySettlementBalance
	aggr  *domain.DailySettlementBalance
}

func newMockBalancerStore() *mockBalancerStore {
	return &mockBalancerStore{
		items: make(map[string]*domain.DailySettlementBalance),
	}
}

func (m *mockBalancerStore) UpsertDailyBalance(_ context.Context, bal *domain.DailySettlementBalance) error {
	key := bal.PropertyID.String() + "|" + bal.ReconDate.Format("2006-01-02")
	m.items[key] = bal
	return nil
}

func (m *mockBalancerStore) GetDailyBalance(_ context.Context, propertyID uuid.UUID, reconDate time.Time) (*domain.DailySettlementBalance, error) {
	key := propertyID.String() + "|" + reconDate.Format("2006-01-02")
	bal, ok := m.items[key]
	if !ok {
		return nil, errors.New("not found")
	}
	return bal, nil
}

func (m *mockBalancerStore) ListDailyBalances(_ context.Context, propertyID uuid.UUID, limit, offset int) ([]*domain.DailySettlementBalance, error) {
	var res []*domain.DailySettlementBalance
	for _, b := range m.items {
		if b.PropertyID == propertyID {
			res = append(res, b)
		}
	}
	return res, nil
}

func (m *mockBalancerStore) ComputeDayAggregates(_ context.Context, propertyID uuid.UUID, reconDate time.Time) (*domain.DailySettlementBalance, error) {
	if m.aggr != nil {
		m.aggr.PropertyID = propertyID
		m.aggr.ReconDate = reconDate
		m.aggr.EvaluateBalance()
		return m.aggr, nil
	}
	bal := &domain.DailySettlementBalance{
		PropertyID:             propertyID,
		ReconDate:              reconDate,
		GatewayGrossPaise:      100000,
		GatewayNetSettledPaise: 97820,
		GatewayFeesPaise:       1850,
		GatewayTaxPaise:        330,
		GatewayAdjustmentPaise: 0,
		GatewayInTransitPaise:  50000,
		BankCreditsPaise:       97820,
		BankDebitsPaise:        0,
		LedgerBankDrPaise:      97820,
		LedgerBankCrPaise:      0,
	}
	bal.EvaluateBalance()
	return bal, nil
}

func TestSettlementBalancer_PerfectBalance(t *testing.T) {
	ctx := context.Background()
	store := newMockBalancerStore()
	balancer := NewSettlementBalancer(store)

	pid := uuid.New()
	date := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

	bal, err := balancer.RunDailyBalance(ctx, pid, date)
	if err != nil {
		t.Fatalf("unexpected error running daily balance: %v", err)
	}

	if !bal.IsBalanced {
		t.Errorf("expected is_balanced=true, got false (discrepancy: %d paise)", bal.DiscrepancyPaise)
	}
	if bal.DiscrepancyPaise != 0 {
		t.Errorf("expected discrepancy_paise=0, got %d", bal.DiscrepancyPaise)
	}

	// Verify persistence
	got, err := balancer.GetDailyBalance(ctx, pid, date)
	if err != nil {
		t.Fatalf("get daily balance: %v", err)
	}
	if got.GatewayGrossPaise != 100000 {
		t.Errorf("expected gross=100000, got %d", got.GatewayGrossPaise)
	}
}

func TestSettlementBalancer_GatewayImbalanceDetected(t *testing.T) {
	ctx := context.Background()
	store := newMockBalancerStore()
	store.aggr = &domain.DailySettlementBalance{
		GatewayGrossPaise:      100000,
		GatewayNetSettledPaise: 95000, // Deficit of 2820 paise
		GatewayFeesPaise:       1850,
		GatewayTaxPaise:        330,
		GatewayAdjustmentPaise: 0,
		BankCreditsPaise:       95000,
		LedgerBankDrPaise:      95000,
	}

	balancer := NewSettlementBalancer(store)
	pid := uuid.New()
	date := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

	bal, err := balancer.RunDailyBalance(ctx, pid, date)
	if err != nil {
		t.Fatalf("run daily balance: %v", err)
	}

	if bal.IsBalanced {
		t.Errorf("expected is_balanced=false due to gateway imbalance")
	}
	if bal.DiscrepancyPaise != 2820 {
		t.Errorf("expected discrepancy=2820, got %d", bal.DiscrepancyPaise)
	}

	foundCategory := false
	for _, d := range bal.Discrepancies {
		if d.Category == "gateway_settlement_imbalance" {
			foundCategory = true
			if d.Severity != "critical" {
				t.Errorf("expected severity=critical, got %s", d.Severity)
			}
		}
	}
	if !foundCategory {
		t.Errorf("expected discrepancy item for gateway_settlement_imbalance")
	}
}

func TestSettlementBalancer_BankVsLedgerDrift(t *testing.T) {
	ctx := context.Background()
	store := newMockBalancerStore()
	store.aggr = &domain.DailySettlementBalance{
		GatewayGrossPaise:      100000,
		GatewayNetSettledPaise: 97820,
		GatewayFeesPaise:       1850,
		GatewayTaxPaise:        330,
		BankCreditsPaise:       150000, // Statement shows 150000
		LedgerBankDrPaise:      97820,  // Ledger only recorded 97820 (52180 drift)
	}

	balancer := NewSettlementBalancer(store)
	pid := uuid.New()
	date := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)

	bal, err := balancer.RunDailyBalance(ctx, pid, date)
	if err != nil {
		t.Fatalf("run daily balance: %v", err)
	}

	if bal.IsBalanced {
		t.Errorf("expected is_balanced=false due to bank vs ledger drift")
	}
	if bal.DiscrepancyPaise != 52180 {
		t.Errorf("expected discrepancy=52180, got %d", bal.DiscrepancyPaise)
	}
}
