package gamification

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockTenantReader struct {
	tenants map[uuid.UUID]*domain.Tenant
}

func (m *mockTenantReader) GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	t, ok := m.tenants[id]
	if !ok {
		return nil, ErrTenantNotFound
	}
	return t, nil
}

func (m *mockTenantReader) Update(ctx context.Context, t *domain.Tenant) error {
	m.tenants[t.ID] = t
	return nil
}

type mockPublisher struct{}

func (p *mockPublisher) Publish(ctx context.Context, evt domain.Event) error {
	return nil
}

// TestCalculateMonthlyRewardBudgetPaise_FormulaAndClamping verifies empirical first-principles math:
// 100 tenants * Rs 5,500 = Rs 5,50,000 rent roll -> 1.5% pool = Rs 8,250 (825,000 paise).
// 500 tenants * Rs 5,500 = Rs 27,50,000 rent roll -> 1.5% pool = Rs 41,250 (4,125,000 paise).
// Ceiling clamps pool if basis points exceed ceiling.
// Zero rent roll falls back to MonthlyBudgetPaise.
func TestCalculateMonthlyRewardBudgetPaise_FormulaAndClamping(t *testing.T) {
	s := &domain.PropertyGamificationSettings{
		RewardBudgetBasisPoints:        150, // 1.50%
		RewardBudgetCeilingBasisPoints: 200, // 2.00%
		MonthlyBudgetPaise:             1000000,
	}

	// Case 1: 100 tenants at Rs 5,500/month = Rs 5,50,000 (55,000,000 paise)
	rentRoll100 := int64(55000000)
	budget100 := domain.CalculateMonthlyRewardBudgetPaise(rentRoll100, s)
	expected100 := int64(825000) // Rs 8,250
	if budget100 != expected100 {
		t.Fatalf("expected 100-tenant budget of %d paise (Rs 8,250), got %d paise", expected100, budget100)
	}

	// Case 2: 500 tenants at Rs 5,500/month = Rs 27,50,000 (275,000,000 paise)
	rentRoll500 := int64(275000000)
	budget500 := domain.CalculateMonthlyRewardBudgetPaise(rentRoll500, s)
	expected500 := int64(4125000) // Rs 41,250
	if budget500 != expected500 {
		t.Fatalf("expected 500-tenant budget of %d paise (Rs 41,250), got %d paise", expected500, budget500)
	}

	// Case 3: Owner configures basis points above ceiling (e.g. 250 bp = 2.5%, ceiling = 200 bp = 2.0%)
	sAboveCeiling := &domain.PropertyGamificationSettings{
		RewardBudgetBasisPoints:        250,
		RewardBudgetCeilingBasisPoints: 200,
	}
	budgetClamped := domain.CalculateMonthlyRewardBudgetPaise(rentRoll100, sAboveCeiling)
	expectedCeiling := int64(1100000) // 2.0% of Rs 5,50,000 = Rs 11,000 (1,100,000 paise)
	if budgetClamped != expectedCeiling {
		t.Fatalf("expected clamped budget of %d paise (Rs 11,000), got %d paise", expectedCeiling, budgetClamped)
	}

	// Case 4: Zero rent roll falls back to MonthlyBudgetPaise
	budgetZeroRoll := domain.CalculateMonthlyRewardBudgetPaise(0, s)
	if budgetZeroRoll != s.MonthlyBudgetPaise {
		t.Fatalf("expected zero rent roll fallback to %d paise, got %d", s.MonthlyBudgetPaise, budgetZeroRoll)
	}

	// Case 5: Nil settings returns 0
	if domain.CalculateMonthlyRewardBudgetPaise(rentRoll100, nil) != 0 {
		t.Fatalf("expected nil settings to return 0")
	}

	// Case 6: Unset basis points use defaults (150 bp and 200 bp ceiling)
	sDefaults := &domain.PropertyGamificationSettings{}
	budgetDefaults := domain.CalculateMonthlyRewardBudgetPaise(rentRoll100, sDefaults)
	if budgetDefaults != expected100 {
		t.Fatalf("expected defaults to compute 1.5%% pool (%d paise), got %d", expected100, budgetDefaults)
	}
}

// TestAwardPoints_RentRollBudgetCap verifies that AwardPoints enforces the dynamic rent roll budget.
func TestAwardPoints_RentRollBudgetCap(t *testing.T) {
	ctx := context.Background()
	propID := uuid.New()
	tenantID := uuid.New()

	mem := newMemStore()
	mem.rentRoll = 55000000 // 100 tenants * Rs 5,500 = Rs 5,50,000 rent roll
	// 1.5% pool = Rs 8,250 = 8,250 points at PointValuePaise = 100.
	mem.settings.PropertyID = propID
	mem.settings.RewardBudgetBasisPoints = 150
	mem.settings.RewardBudgetCeilingBasisPoints = 200
	mem.settings.PointValuePaise = 100

	tenants := &mockTenantReader{
		tenants: map[uuid.UUID]*domain.Tenant{
			tenantID: {
				ID:         tenantID,
				PropertyID: propID,
				Status:     domain.TenantStatusActive,
				RentAmount: 550000,
			},
		},
	}

	svc := NewService(mem, tenants, nil, &mockPublisher{}, nil)
	fixedTime := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return fixedTime }

	// Seed property month points to 8,220 (leaving 30 points before 8,250 budget)
	monthKeyStr := monthKey(fixedTime)
	for i := 0; i < 822; i++ {
		mem.ledger = append(mem.ledger, domain.PointsLedgerEntry{
			PropertyID: propID,
			TenantID:   uuid.New(),
			RuleCode:   "ROOM_CLEAN",
			Delta:      10,
			CreatedAt:  fixedTime,
		})
	}
	earned, err := mem.GetPropertyMonthPoints(ctx, propID, monthKeyStr)
	if err != nil || earned != 8220 {
		t.Fatalf("expected 8220 earned points, got %d (err: %v)", earned, err)
	}

	// Rule for 30 points: exactly reaches 8,250 budget -> succeeds
	mem.rules["TEST_30"] = domain.PointRule{Code: "TEST_30", Points: 30, Active: true}
	awarded, err := svc.AwardPoints(ctx, tenantID, "TEST_30", nil, nil, nil)
	if err != nil {
		t.Fatalf("expected award of 30 points to succeed, got error: %v", err)
	}
	if awarded != 30 {
		t.Fatalf("expected 30 awarded points, got %d", awarded)
	}

	// Next award of 15 points exceeds 8,250 budget -> must fail closed with ErrPropertyBudgetExceeded
	awarded, err = svc.AwardPoints(ctx, tenantID, "ROOM_CLEAN", nil, nil, nil)
	if !errors.Is(err, ErrPropertyBudgetExceeded) {
		t.Fatalf("expected ErrPropertyBudgetExceeded, got awarded=%d, err=%v", awarded, err)
	}
}

// TestAwardPoints_EarnCap100 verifies that the lowered 100-point tenant earn cap is enforced.
func TestAwardPoints_EarnCap100(t *testing.T) {
	ctx := context.Background()
	propID := uuid.New()
	tenantID := uuid.New()

	mem := newMemStore()
	mem.rentRoll = 55000000
	mem.settings.PropertyID = propID
	mem.settings.EarnCapPerTenant = 100 // Lowered from 200 to 100 points

	tenants := &mockTenantReader{
		tenants: map[uuid.UUID]*domain.Tenant{
			tenantID: {
				ID:         tenantID,
				PropertyID: propID,
				Status:     domain.TenantStatusActive,
				RentAmount: 550000,
			},
		},
	}

	svc := NewService(mem, tenants, nil, &mockPublisher{}, nil)
	fixedTime := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return fixedTime }

	// Award 50 points on-time rent
	awarded, err := svc.AwardPoints(ctx, tenantID, "RENT_ON_TIME", nil, nil, nil)
	if err != nil || awarded != 50 {
		t.Fatalf("expected 50 points awarded, got %d, err=%v", awarded, err)
	}

	// Award 50 points energy saver -> total 100 points (cap reached)
	awarded, err = svc.AwardPoints(ctx, tenantID, "ENERGY_SAVER_MONTH", nil, nil, nil)
	if err != nil || awarded != 50 {
		t.Fatalf("expected 50 points awarded, got %d, err=%v", awarded, err)
	}

	// Attempt to award another 15 points -> must be rejected by the 100-point cap
	awarded, err = svc.AwardPoints(ctx, tenantID, "ROOM_CLEAN", nil, nil, nil)
	if !errors.Is(err, ErrMonthlyCapExceeded) {
		t.Fatalf("expected ErrMonthlyCapExceeded at 100 points, got awarded=%d, err=%v", awarded, err)
	}
}

// TestAwardPoints_FailClosedOnInvalidPointValue verifies that non-positive point values fail closed.
func TestAwardPoints_FailClosedOnInvalidPointValue(t *testing.T) {
	ctx := context.Background()
	propID := uuid.New()
	tenantID := uuid.New()

	mem := newMemStore()
	mem.rentRoll = 55000000
	mem.settings.PropertyID = propID
	mem.settings.PointValuePaise = 0 // Bad configuration: 0 paise per point

	tenants := &mockTenantReader{
		tenants: map[uuid.UUID]*domain.Tenant{
			tenantID: {
				ID:         tenantID,
				PropertyID: propID,
				Status:     domain.TenantStatusActive,
			},
		},
	}

	svc := NewService(mem, tenants, nil, &mockPublisher{}, nil)
	_, err := svc.AwardPoints(ctx, tenantID, "RENT_ON_TIME", nil, nil, nil)
	if !errors.Is(err, ErrInvalidRewardValue) {
		t.Fatalf("expected ErrInvalidRewardValue when PointValuePaise=0, got: %v", err)
	}
}

// TestAwardPoints_CustomPropertyBudget verifies per-property customization (1.0% pool, 1.2% ceiling).
func TestAwardPoints_CustomPropertyBudget(t *testing.T) {
	ctx := context.Background()
	propID := uuid.New()
	tenantID := uuid.New()

	mem := newMemStore()
	mem.rentRoll = 55000000 // Rs 5,50,000
	mem.settings.PropertyID = propID
	mem.settings.RewardBudgetBasisPoints = 100        // 1.0% pool = Rs 5,500 = 5,500 points
	mem.settings.RewardBudgetCeilingBasisPoints = 120 // 1.2% ceiling = Rs 6,600
	mem.settings.PointValuePaise = 100

	tenants := &mockTenantReader{
		tenants: map[uuid.UUID]*domain.Tenant{
			tenantID: {
				ID:         tenantID,
				PropertyID: propID,
				Status:     domain.TenantStatusActive,
			},
		},
	}

	svc := NewService(mem, tenants, nil, &mockPublisher{}, nil)
	fixedTime := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return fixedTime }

	// Seed 5,490 points
	for i := 0; i < 549; i++ {
		mem.ledger = append(mem.ledger, domain.PointsLedgerEntry{
			PropertyID: propID,
			TenantID:   uuid.New(),
			RuleCode:   "ROOM_CLEAN",
			Delta:      10,
			CreatedAt:  fixedTime,
		})
	}

	// 15-point award would reach 5,505 points (> 5,500 budget) -> fails
	_, err := svc.AwardPoints(ctx, tenantID, "ROOM_CLEAN", nil, nil, nil)
	if !errors.Is(err, ErrPropertyBudgetExceeded) {
		t.Fatalf("expected ErrPropertyBudgetExceeded for custom 1.0%% budget, got: %v", err)
	}
}

// TestAwardPoints_ConcurrentBudgetNoOvershoot verifies that concurrent point awards
// do not overshoot the property budget under concurrent contention.
func TestAwardPoints_ConcurrentBudgetNoOvershoot(t *testing.T) {
	ctx := context.Background()
	propID := uuid.New()

	mem := newMemStore()
	mem.rentRoll = 55000000 // 100 tenants * Rs 5,500 = Rs 5,50,000 rent roll
	// 1.5% pool = Rs 8,250 = 8,250 points at PointValuePaise = 100
	mem.settings.PropertyID = propID
	mem.settings.RewardBudgetBasisPoints = 150
	mem.settings.RewardBudgetCeilingBasisPoints = 200
	mem.settings.PointValuePaise = 100
	mem.settings.EarnCapPerTenant = 100
	mem.delayQuery = 5 * time.Millisecond

	tenantsMap := make(map[uuid.UUID]*domain.Tenant)
	concurrency := 20
	tenantIDs := make([]uuid.UUID, concurrency)
	for i := 0; i < concurrency; i++ {
		tid := uuid.New()
		tenantIDs[i] = tid
		tenantsMap[tid] = &domain.Tenant{
			ID:         tid,
			PropertyID: propID,
			Status:     domain.TenantStatusActive,
			RentAmount: 550000,
		}
	}
	tenants := &mockTenantReader{tenants: tenantsMap}
	svc := NewService(mem, tenants, nil, &mockPublisher{}, nil)
	fixedTime := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return fixedTime }

	// Seed property points to 8,240 (leaving exactly 10 points before 8,250 cap)
	for i := 0; i < 824; i++ {
		mem.ledger = append(mem.ledger, domain.PointsLedgerEntry{
			PropertyID: propID,
			TenantID:   uuid.New(),
			RuleCode:   "ROOM_CLEAN",
			Delta:      10,
			CreatedAt:  fixedTime,
		})
	}

	mem.rules["TEST_10"] = domain.PointRule{Code: "TEST_10", Points: 10, Active: true}

	var wg sync.WaitGroup
	startCh := make(chan struct{})
	successCount := int64(0)
	budgetExceededCount := int64(0)
	errCh := make(chan error, concurrency)

	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func(tid uuid.UUID) {
			defer wg.Done()
			<-startCh // Synchronize start
			_, err := svc.AwardPoints(ctx, tid, "TEST_10", nil, nil, nil)
			if err == nil {
				atomic.AddInt64(&successCount, 1)
			} else if errors.Is(err, ErrPropertyBudgetExceeded) {
				atomic.AddInt64(&budgetExceededCount, 1)
			} else {
				errCh <- err
			}
		}(tenantIDs[i])
	}

	close(startCh)
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("unexpected error during award: %v", err)
	}

	monthKeyStr := monthKey(fixedTime)
	totalEarned, err := mem.GetPropertyMonthPoints(ctx, propID, monthKeyStr)
	if err != nil {
		t.Fatalf("failed to get property month points: %v", err)
	}

	if totalEarned > 8250 {
		t.Fatalf("BUDGET OVERSHOOT DETECTED: expected <= 8250 points, got %d points (successes=%d)", totalEarned, successCount)
	}

	if successCount != 1 {
		t.Fatalf("expected exactly 1 success for the remaining 10 points, got %d (overshoot/undershoot)", successCount)
	}
	if budgetExceededCount != int64(concurrency-1) {
		t.Fatalf("expected %d rejections with ErrPropertyBudgetExceeded, got %d", concurrency-1, budgetExceededCount)
	}
}
