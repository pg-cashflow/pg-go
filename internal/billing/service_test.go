package billing

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/qr"
)

type mockDueRepo struct {
	mu           sync.Mutex
	dues         map[uuid.UUID]*domain.Due
	byCode       map[string]uuid.UUID
	createCalls  int
	failOnCode   string
	simulateColl int
}

func newMockDueRepo() *mockDueRepo {
	return &mockDueRepo{
		dues:   make(map[uuid.UUID]*domain.Due),
		byCode: make(map[string]uuid.UUID),
	}
}

func (r *mockDueRepo) Create(ctx context.Context, d *domain.Due) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.createCalls++

	if r.simulateColl > 0 {
		r.simulateColl--
		return qr.ErrConflict
	}
	if r.failOnCode != "" && d.DueCode == r.failOnCode {
		return qr.ErrConflict
	}
	if _, exists := r.byCode[d.DueCode]; exists {
		return qr.ErrConflict
	}

	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	r.byCode[d.DueCode] = d.ID
	cp := *d
	r.dues[d.ID] = &cp
	return nil
}

func (r *mockDueRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Due, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	d, ok := r.dues[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *d
	return &cp, nil
}

func (r *mockDueRepo) Update(ctx context.Context, d *domain.Due) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *d
	r.dues[d.ID] = &cp
	return nil
}

func (r *mockDueRepo) HasOpenRentDue(ctx context.Context, tenantID uuid.UUID) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, d := range r.dues {
		if d.TenantID == tenantID && d.Kind == domain.DueKindRent && d.Status == domain.DueStatusPending {
			return true, nil
		}
	}
	return false, nil
}

func (r *mockDueRepo) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Due, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var list []domain.Due
	for _, d := range r.dues {
		if d.TenantID == tenantID {
			list = append(list, *d)
		}
	}
	return list, nil
}

type mockTenantRepo struct {
	mu      sync.Mutex
	rowLock sync.Mutex
	tenants map[uuid.UUID]*domain.Tenant
}

func newMockTenantRepo() *mockTenantRepo {
	return &mockTenantRepo{
		tenants: make(map[uuid.UUID]*domain.Tenant),
	}
}

func (r *mockTenantRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tenants[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	cp := *t
	return &cp, nil
}

func (r *mockTenantRepo) GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	// Simulate row-level locking (SELECT ... FOR UPDATE)
	r.rowLock.Lock()
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tenants[id]
	if !ok {
		r.rowLock.Unlock()
		return nil, domain.ErrNotFound
	}
	cp := *t
	return &cp, nil
}

func (r *mockTenantRepo) ReleaseRowLock() {
	r.rowLock.Unlock()
}

func (r *mockTenantRepo) DeductCredit(ctx context.Context, tenantID uuid.UUID, amountPaise int64) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tenants[tenantID]
	if !ok {
		return 0, domain.ErrNotFound
	}
	t.CreditBalancePaise -= amountPaise
	if t.CreditBalancePaise < 0 {
		t.CreditBalancePaise = 0
	}
	return t.CreditBalancePaise, nil
}

func (r *mockTenantRepo) AddCredit(tenantID uuid.UUID, amountPaise int64) {
	r.rowLock.Lock()
	defer r.rowLock.Unlock()
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.tenants[tenantID]
	t.CreditBalancePaise += amountPaise
}

func (r *mockTenantRepo) Update(ctx context.Context, t *domain.Tenant) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *t
	r.tenants[t.ID] = &cp
	return nil
}

type mockPub struct {
	mu     sync.Mutex
	events []domain.Event
}

func (p *mockPub) Publish(ctx context.Context, e domain.Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
	return nil
}

func TestBillingService_DueCodeCollisionInsideTx(t *testing.T) {
	dues := newMockDueRepo()
	tenants := newMockTenantRepo()
	pub := &mockPub{}

	tenantID := uuid.New()
	propID := uuid.New()
	tenant := &domain.Tenant{
		ID:                 tenantID,
		PropertyID:         propID,
		RentAmount:         1000000,
		CreditBalancePaise: 0,
	}
	tenants.tenants[tenantID] = tenant

	// Simulate 1 collision on due code creation
	dues.simulateColl = 1

	svc := NewService(dues, tenants, pub)
	svc.runInTx = func(ctx context.Context, fn func(DueRepository, TenantRepository, events.Publisher) error) error {
		defer tenants.ReleaseRowLock()
		return fn(dues, tenants, pub)
	}

	due, err := svc.CreateRentDue(context.Background(), tenant)
	if err != nil {
		t.Fatalf("expected CreateRentDue to succeed with collision retry, got: %v", err)
	}

	if dues.createCalls < 2 {
		t.Fatalf("expected at least 2 create calls due to collision retry, got: %d", dues.createCalls)
	}

	if due.DueCode == "" {
		t.Fatalf("expected non-empty due code after retry")
	}

	if due.Amount != 1000000 {
		t.Fatalf("expected due amount 1000000, got: %d", due.Amount)
	}
}

func TestBillingService_CreditAddedByWebhookWhileBillingRuns(t *testing.T) {
	dues := newMockDueRepo()
	tenants := newMockTenantRepo()
	pub := &mockPub{}

	tenantID := uuid.New()
	propID := uuid.New()
	tenant := &domain.Tenant{
		ID:                 tenantID,
		PropertyID:         propID,
		RentAmount:         1000000, // Rs 10,000
		CreditBalancePaise: 0,
	}
	tenants.tenants[tenantID] = tenant

	svc := NewService(dues, tenants, pub)

	// Step A: Webhook lands credit before billing transaction starts
	tenants.AddCredit(tenantID, 400000) // Rs 4,000 credit

	svc.runInTx = func(ctx context.Context, fn func(DueRepository, TenantRepository, events.Publisher) error) error {
		defer tenants.ReleaseRowLock()
		return fn(dues, tenants, pub)
	}

	due, err := svc.CreateRentDue(context.Background(), tenant)
	if err != nil {
		t.Fatalf("expected CreateRentDue to succeed, got: %v", err)
	}

	// Due amount must be reduced by credit (10,000 - 4,000 = 6,000)
	if due.Amount != 600000 {
		t.Fatalf("expected due amount to be 600000 paise, got: %d", due.Amount)
	}

	// Tenant credit balance must be 0 after application
	storedTenant := tenants.tenants[tenantID]
	if storedTenant.CreditBalancePaise != 0 {
		t.Fatalf("expected remaining credit balance 0, got: %d", storedTenant.CreditBalancePaise)
	}

	// Step B: Concurrency verification with row lock
	// While billing holds row lock, a webhook attempting AddCredit must wait until billing commits
	billingStarted := make(chan struct{})
	billingHold := make(chan struct{})
	webhookDone := make(chan struct{})

	svc.runInTx = func(ctx context.Context, fn func(DueRepository, TenantRepository, events.Publisher) error) error {
		defer tenants.ReleaseRowLock()
		err := fn(dues, tenants, pub)
		close(billingStarted)
		<-billingHold
		return err
	}

	tenant2 := &domain.Tenant{
		ID:                 uuid.New(),
		PropertyID:         propID,
		RentAmount:         500000,
		CreditBalancePaise: 100000,
	}
	tenants.tenants[tenant2.ID] = tenant2

	go func() {
		<-billingStarted
		// Webhook tries to add credit while billing is holding the row lock
		tenants.AddCredit(tenant2.ID, 200000)
		close(webhookDone)
	}()

	// Launch second billing cycle in background
	go func() {
		_, _ = svc.CreateRentDue(context.Background(), tenant2)
	}()

	// Ensure webhook is blocked until billingHold is released
	<-billingStarted
	select {
	case <-webhookDone:
		t.Fatalf("CRITICAL CONCURRENCY BUG: webhook did not wait for billing row lock!")
	case <-time.After(50 * time.Millisecond):
		// Expected: webhook is waiting for row lock
	}

	// Release billing transaction
	close(billingHold)
	<-webhookDone

	// Final credit must include webhook's addition without being overwritten
	finalTenant := tenants.tenants[tenant2.ID]
	if finalTenant.CreditBalancePaise != 200000 {
		t.Fatalf("expected final tenant credit 200000, got: %d", finalTenant.CreditBalancePaise)
	}
}

func TestBillingService_FullCreditMarksDuePaid(t *testing.T) {
	dues := newMockDueRepo()
	tenants := newMockTenantRepo()
	pub := &mockPub{}

	tenantID := uuid.New()
	propID := uuid.New()
	tenant := &domain.Tenant{
		ID:                 tenantID,
		PropertyID:         propID,
		RentAmount:         500000, // Rs 5,000
		CreditBalancePaise: 700000, // Rs 7,000 credit
	}
	tenants.tenants[tenantID] = tenant

	svc := NewService(dues, tenants, pub)
	svc.runInTx = func(ctx context.Context, fn func(DueRepository, TenantRepository, events.Publisher) error) error {
		defer tenants.ReleaseRowLock()
		return fn(dues, tenants, pub)
	}

	due, err := svc.CreateRentDue(context.Background(), tenant)
	if err != nil {
		t.Fatalf("expected CreateRentDue to succeed, got: %v", err)
	}

	if due.Status != domain.DueStatusPaid {
		t.Fatalf("expected due to be marked paid, got status: %s", due.Status)
	}
	if due.Amount != 0 {
		t.Fatalf("expected due amount to be 0, got: %d", due.Amount)
	}

	remaining := tenants.tenants[tenantID].CreditBalancePaise
	if remaining != 200000 {
		t.Fatalf("expected remaining credit balance 200000 (700000-500000), got: %d", remaining)
	}
}
