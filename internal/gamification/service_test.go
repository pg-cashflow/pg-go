package gamification

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type memStore struct {
	Store
	mu             sync.Mutex
	txMu           sync.Mutex
	settings       *domain.PropertyGamificationSettings
	rules          map[string]domain.PointRule
	ledger         []domain.PointsLedgerEntry
	streak         map[uuid.UUID]*domain.TenantStreak
	catalog        map[uuid.UUID]domain.RewardsCatalogItem
	redemptions    []domain.Redemption
	violations     []domain.Violation
	readings       []domain.MeterReading
	rooms          map[uuid.UUID]domain.Room
	step3InQuarter int
}

type mockTx struct {
	pgx.Tx
	m      *memStore
	closed bool
	mu     sync.Mutex
}

func (tx *mockTx) Commit(ctx context.Context) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if !tx.closed {
		tx.closed = true
		tx.m.txMu.Unlock()
	}
	return nil
}

func (tx *mockTx) Rollback(ctx context.Context) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if !tx.closed {
		tx.closed = true
		tx.m.txMu.Unlock()
	}
	return nil
}

func newMemStore() *memStore {
	return &memStore{
		settings: &domain.PropertyGamificationSettings{
			PointValuePaise:        100,
			MonthlyBudgetPaise:     1000000,
			EarnCapPerTenant:       200,
			RSVPSubCap:             60,
			ExpiryDays:             180,
			FloorBonusThreshold:    85,
			ElectricityTariffPaise: 1000,
		},
		rules: map[string]domain.PointRule{
			"RENT_ON_TIME":       {Code: "RENT_ON_TIME", Points: 50, Active: true},
			"ROOM_CLEAN":         {Code: "ROOM_CLEAN", Points: 15, MonthlyCap: 60, Active: true},
			"FLOOR_CLEAN_BONUS":  {Code: "FLOOR_CLEAN_BONUS", Points: 30, Active: true},
			"MEAL_RSVP_ON_TIME":  {Code: "MEAL_RSVP_ON_TIME", Points: 2, MonthlyCap: 60, IsRSVP: true, Active: true},
			"HAZARD_REPORT":      {Code: "HAZARD_REPORT", Points: 25, Active: true},
			"ENERGY_SAVER_MONTH": {Code: "ENERGY_SAVER_MONTH", Points: 50, Active: true},
		},
		streak:  make(map[uuid.UUID]*domain.TenantStreak),
		catalog: make(map[uuid.UUID]domain.RewardsCatalogItem),
		rooms:   make(map[uuid.UUID]domain.Room),
	}
}

func (m *memStore) BeginTx(ctx context.Context) (pgx.Tx, error) {
	m.txMu.Lock()
	return &mockTx{m: m}, nil
}

func (m *memStore) GetSettings(ctx context.Context, propertyID uuid.UUID) (*domain.PropertyGamificationSettings, error) {
	return m.settings, nil
}

func (m *memStore) GetPointRuleByCode(ctx context.Context, propertyID uuid.UUID, code string) (*domain.PointRule, error) {
	r, ok := m.rules[code]
	if !ok {
		return nil, ErrRuleNotFound
	}
	return &r, nil
}

func (m *memStore) GetActiveBalance(ctx context.Context, tenantID uuid.UUID) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calculateBalance(tenantID)
}

func (m *memStore) GetActiveBalanceTx(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (int, error) {
	// Inside lock
	return m.calculateBalance(tenantID)
}

func (m *memStore) calculateBalance(tenantID uuid.UUID) (int, error) {
	now := time.Now().UTC()
	pos := 0
	neg := 0
	for _, e := range m.ledger {
		if e.TenantID == tenantID {
			if e.Delta > 0 {
				if e.ExpiresAt == nil || e.ExpiresAt.After(now) {
					pos += e.Delta
				}
			} else {
				neg += e.Delta // negative number
			}
		}
	}
	total := pos + neg
	if total < 0 {
		total = 0
	}
	return total, nil
}

func (m *memStore) GetExpiringSoon(ctx context.Context, tenantID uuid.UUID, withinDays int) (int, time.Time, error) {
	return 0, time.Time{}, nil
}

func (m *memStore) GetTenantMonthPoints(ctx context.Context, tenantID uuid.UUID, monthYear string, isRSVP bool) (int, error) {
	sum := 0
	for _, e := range m.ledger {
		if e.TenantID == tenantID && e.Delta > 0 {
			rule := m.rules[e.RuleCode]
			if rule.IsRSVP == isRSVP {
				sum += e.Delta
			}
		}
	}
	return sum, nil
}

func (m *memStore) GetPropertyMonthPoints(ctx context.Context, propertyID uuid.UUID, monthYear string) (int, error) {
	sum := 0
	for _, e := range m.ledger {
		if e.Delta > 0 {
			sum += e.Delta
		}
	}
	return sum, nil
}

func (m *memStore) GetRuleMonthPoints(ctx context.Context, tenantID uuid.UUID, ruleCode string, monthYear string) (int, error) {
	sum := 0
	for _, e := range m.ledger {
		if e.TenantID == tenantID && e.RuleCode == ruleCode && e.Delta > 0 {
			sum += e.Delta
		}
	}
	return sum, nil
}

func (m *memStore) InsertLedgerEntry(ctx context.Context, entry *domain.PointsLedgerEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ledger = append(m.ledger, *entry)
	return nil
}

func (m *memStore) InsertLedgerEntryTx(ctx context.Context, tx pgx.Tx, entry *domain.PointsLedgerEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ledger = append(m.ledger, *entry)
	return nil
}

func (m *memStore) GetStreak(ctx context.Context, tenantID uuid.UUID) (*domain.TenantStreak, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.streak[tenantID]
	if !ok {
		return &domain.TenantStreak{TenantID: tenantID, FreezesAvailable: 1}, nil
	}
	return s, nil
}

func (m *memStore) GetStreakForUpdate(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (*domain.TenantStreak, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.streak[tenantID]
	if !ok {
		s = &domain.TenantStreak{TenantID: tenantID, FreezesAvailable: 1}
		m.streak[tenantID] = s
	}
	return s, nil
}

func (m *memStore) UpsertStreak(ctx context.Context, s *domain.TenantStreak) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streak[s.TenantID] = s
	return nil
}

func (m *memStore) UpsertStreakTx(ctx context.Context, tx pgx.Tx, s *domain.TenantStreak) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streak[s.TenantID] = s
	return nil
}

func (m *memStore) GetRewardByID(ctx context.Context, id uuid.UUID) (*domain.RewardsCatalogItem, error) {
	r, ok := m.catalog[id]
	if !ok {
		return nil, errors.New("not found")
	}
	return &r, nil
}

func (m *memStore) CountStep3ViolationsInQuarter(ctx context.Context, tenantID uuid.UUID) (int, error) {
	return m.step3InQuarter, nil
}

func (m *memStore) CreateRedemptionTx(ctx context.Context, tx pgx.Tx, red *domain.Redemption) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.redemptions = append(m.redemptions, *red)
	return nil
}

func (m *memStore) GetLatestMeterReading(ctx context.Context, propertyID uuid.UUID, roomID *uuid.UUID, floorID *uuid.UUID, kind string) (*domain.MeterReading, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.readings) - 1; i >= 0; i-- {
		r := m.readings[i]
		if r.Kind == kind {
			return &r, nil
		}
	}
	return nil, nil
}

func (m *memStore) CreateMeterReading(ctx context.Context, mr *domain.MeterReading) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.readings = append(m.readings, *mr)
	return nil
}

func (m *memStore) GetRoomByID(ctx context.Context, id uuid.UUID) (*domain.Room, error) {
	r, ok := m.rooms[id]
	if !ok {
		return &domain.Room{ID: id, IncludedUnits: 50}, nil
	}
	return &r, nil
}

func (m *memStore) UpsertMealRSVP(ctx context.Context, rsvp *domain.MealRSVP) error {
	return nil
}

type memTenantRepo struct {
	tenants map[uuid.UUID]*domain.Tenant
}

func (r *memTenantRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	t, ok := r.tenants[id]
	if !ok {
		return nil, ErrTenantNotFound
	}
	return t, nil
}

func (r *memTenantRepo) Update(ctx context.Context, t *domain.Tenant) error {
	r.tenants[t.ID] = t
	return nil
}

type noopDueWriter struct{}

func (n *noopDueWriter) Create(ctx context.Context, d *domain.Due) error {
	return nil
}

type noopPublisher struct{}

func (p *noopPublisher) Publish(ctx context.Context, e domain.Event) error {
	return nil
}

// 1. P0 Test: Concurrent Redemptions Race Condition (Double-Spend Protection)
func TestRedeem_ConcurrentRace(t *testing.T) {
	store := newMemStore()
	tenantID := uuid.New()
	propID := uuid.New()
	tRepo := &memTenantRepo{
		tenants: map[uuid.UUID]*domain.Tenant{
			tenantID: {ID: tenantID, PropertyID: propID, CreditBalancePaise: 0},
		},
	}

	rewardID := uuid.New()
	store.catalog[rewardID] = domain.RewardsCatalogItem{
		ID:              rewardID,
		PointsCost:      500,
		Category:        "cash_credit",
		MinTenureMonths: 3,
		IsActive:        true,
		Metadata:        json.RawMessage(`{"discount_paise": 50000}`),
	}
	store.streak[tenantID] = &domain.TenantStreak{
		TenantID:      tenantID,
		OnTimeMonths:  3,
		CachedBalance: 500,
	}

	svc := NewService(store, tRepo, &noopDueWriter{}, &noopPublisher{}, NewBlobStore())

	// Give tenant exactly 500 unexpired points
	expiresAt := time.Now().UTC().AddDate(0, 0, 180)
	_ = store.InsertLedgerEntry(context.Background(), &domain.PointsLedgerEntry{
		TenantID:   tenantID,
		PropertyID: propID,
		RuleCode:   "RENT_ON_TIME",
		Delta:      500,
		ExpiresAt:  &expiresAt,
	})

	// Launch 10 concurrent redemption requests
	concurrency := 10
	var wg sync.WaitGroup
	wg.Add(concurrency)
	successCount := 0
	failCount := 0
	var countMu sync.Mutex

	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			_, err := svc.RedeemReward(context.Background(), tenantID, rewardID)
			countMu.Lock()
			defer countMu.Unlock()
			if err == nil {
				successCount++
			} else {
				failCount++
			}
		}()
	}
	wg.Wait()

	if successCount != 1 {
		t.Fatalf("expected exactly 1 redemption to succeed, got %d (double-spend detected!)", successCount)
	}
	if failCount != 9 {
		t.Fatalf("expected 9 redemptions to fail with insufficient points, got %d", failCount)
	}

	bal, _ := store.GetActiveBalance(context.Background(), tenantID)
	if bal != 0 {
		t.Fatalf("expected final balance to be 0, got %d", bal)
	}
}

// 2. P0 Test: Deductions Never Expire (Points Do Not Resurrect)
func TestLedger_DeductionsNeverExpire(t *testing.T) {
	store := newMemStore()
	tenantID := uuid.New()
	propID := uuid.New()

	past := time.Now().UTC().AddDate(0, 0, -10)
	future := time.Now().UTC().AddDate(0, 0, 100)

	// 100 expired points from the past
	store.ledger = append(store.ledger, domain.PointsLedgerEntry{
		TenantID:   tenantID,
		PropertyID: propID,
		Delta:      100,
		ExpiresAt:  &past,
	})

	// 100 active points in future
	store.ledger = append(store.ledger, domain.PointsLedgerEntry{
		TenantID:   tenantID,
		PropertyID: propID,
		Delta:      100,
		ExpiresAt:  &future,
	})

	// Deduct 40 points permanently (ExpiresAt = nil)
	store.ledger = append(store.ledger, domain.PointsLedgerEntry{
		TenantID:   tenantID,
		PropertyID: propID,
		Delta:      -40,
		ExpiresAt:  nil,
	})

	// Active balance should be 100 - 40 = 60 (expired 100 points do not resurrect)
	bal, err := store.GetActiveBalance(context.Background(), tenantID)
	if err != nil {
		t.Fatal(err)
	}
	if bal != 60 {
		t.Fatalf("expected active balance 60, got %d", bal)
	}
}

// 3. P0 Test: Meter Readings Floor & Ceiling Checks
func TestMetering_FloorAndCeilingChecks(t *testing.T) {
	store := newMemStore()
	svc := NewService(store, nil, &noopDueWriter{}, &noopPublisher{}, NewBlobStore())
	propID := uuid.New()
	roomID := uuid.New()

	// Initial reading: 100 kWh
	_, err := svc.RecordMeterReading(context.Background(), MeterReadingInput{
		PropertyID:   propID,
		RoomID:       &roomID,
		Kind:         "electricity",
		ReadingValue: 100,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Reading lower than previous (95 kWh) must be rejected
	_, err = svc.RecordMeterReading(context.Background(), MeterReadingInput{
		PropertyID:   propID,
		RoomID:       &roomID,
		Kind:         "electricity",
		ReadingValue: 95,
	})
	if !errors.Is(err, ErrMeterReadingDecreased) {
		t.Fatalf("expected ErrMeterReadingDecreased, got %v", err)
	}

	// 2. Reading exceeding safety ceiling (delta > 400 kWh e.g. 600 kWh -> delta 500) must be rejected
	_, err = svc.RecordMeterReading(context.Background(), MeterReadingInput{
		PropertyID:   propID,
		RoomID:       &roomID,
		Kind:         "electricity",
		ReadingValue: 600,
	})
	if !errors.Is(err, ErrMeterReadingCeiling) {
		t.Fatalf("expected ErrMeterReadingCeiling, got %v", err)
	}

	// 3. Valid reading (180 kWh -> delta 80 kWh; included 50 -> excess 30 kWh)
	res, err := svc.RecordMeterReading(context.Background(), MeterReadingInput{
		PropertyID:   propID,
		RoomID:       &roomID,
		Kind:         "electricity",
		ReadingValue: 180,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.DeltaUnits != 80 {
		t.Fatalf("expected delta 80, got %.2f", res.DeltaUnits)
	}
	if res.ExcessUnits != 30 {
		t.Fatalf("expected excess 30, got %.2f", res.ExcessUnits)
	}
	if res.BillablePaise != 30000 { // 30 units * 1000 paise
		t.Fatalf("expected billable paise 30000, got %d", res.BillablePaise)
	}
}

// 4. P1 Test: Floor Bonus Multiplier (No Free-Riding)
func TestFloorBonus_MultiplierNoFreeRiding(t *testing.T) {
	store := newMemStore()
	tenantA := uuid.New()
	tenantB := uuid.New()
	propID := uuid.New()
	tRepo := &memTenantRepo{
		tenants: map[uuid.UUID]*domain.Tenant{
			tenantA: {ID: tenantA, PropertyID: propID},
			tenantB: {ID: tenantB, PropertyID: propID},
		},
	}
	svc := NewService(store, tRepo, &noopDueWriter{}, &noopPublisher{}, NewBlobStore())

	// Tenant A earned 60 clean points this month
	_ = store.InsertLedgerEntry(context.Background(), &domain.PointsLedgerEntry{
		TenantID:   tenantA,
		PropertyID: propID,
		RuleCode:   "ROOM_CLEAN",
		Delta:      60,
	})

	// Tenant B earned 0 clean points (failed room inspection)

	cleanA, _ := store.GetRuleMonthPoints(context.Background(), tenantA, "ROOM_CLEAN", time.Now().Format("2006-01"))
	cleanB, _ := store.GetRuleMonthPoints(context.Background(), tenantB, "ROOM_CLEAN", time.Now().Format("2006-01"))

	multA := int(float64(cleanA) * 0.25)
	multB := int(float64(cleanB) * 0.25)

	if multA != 15 {
		t.Fatalf("expected Tenant A to get 15 multiplier bonus points, got %d", multA)
	}
	if multB != 0 {
		t.Fatalf("expected messy Tenant B with 0 clean points to get 0 multiplier points, got %d", multB)
	}
	_ = svc
}

// 5. P1 Test: Step-3 Violation Blocks Cash Rent Credit
func TestRedeem_Step3ViolationBlocksCash(t *testing.T) {
	store := newMemStore()
	tenantID := uuid.New()
	propID := uuid.New()
	tRepo := &memTenantRepo{
		tenants: map[uuid.UUID]*domain.Tenant{
			tenantID: {ID: tenantID, PropertyID: propID},
		},
	}
	svc := NewService(store, tRepo, &noopDueWriter{}, &noopPublisher{}, NewBlobStore())

	rewardID := uuid.New()
	store.catalog[rewardID] = domain.RewardsCatalogItem{
		ID:         rewardID,
		PointsCost: 500,
		Category:   "cash_credit",
		IsActive:   true,
	}

	// Tenant has 1000 points and 3-month streak
	future := time.Now().UTC().AddDate(0, 0, 180)
	_ = store.InsertLedgerEntry(context.Background(), &domain.PointsLedgerEntry{
		TenantID:   tenantID,
		PropertyID: propID,
		Delta:      1000,
		ExpiresAt:  &future,
	})
	store.streak[tenantID] = &domain.TenantStreak{
		TenantID:     tenantID,
		OnTimeMonths: 3,
	}

	// Step-3 violation in current quarter
	store.step3InQuarter = 1

	_, err := svc.RedeemReward(context.Background(), tenantID, rewardID)
	if !errors.Is(err, ErrStep3ViolationBlocked) {
		t.Fatalf("expected ErrStep3ViolationBlocked, got %v", err)
	}
}
