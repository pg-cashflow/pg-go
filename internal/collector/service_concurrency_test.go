package collector

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type threadSafeLockIntentStore struct {
	mu          sync.Mutex
	reusable    *domain.PaymentIntent
	superseded  int32
	createCalls int32
}

func (s *threadSafeLockIntentStore) Create(ctx context.Context, p *domain.PaymentIntent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	atomic.AddInt32(&s.createCalls, 1)
	s.reusable = p
	return nil
}

func (s *threadSafeLockIntentStore) LatestOpenForDue(ctx context.Context, dueID uuid.UUID) (*domain.PaymentIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reusable, nil
}

func (s *threadSafeLockIntentStore) GetReusableIntentUnderLock(ctx context.Context, tenantID, dueID uuid.UUID, minRemaining time.Duration) (*domain.PaymentIntent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reusable != nil && s.reusable.ExpiresAt != nil && s.reusable.ExpiresAt.After(time.Now().Add(minRemaining)) {
		return s.reusable, nil
	}
	return nil, nil
}

func (s *threadSafeLockIntentStore) SupersedeOpenIntentsForDue(ctx context.Context, dueID uuid.UUID) error {
	atomic.AddInt32(&s.superseded, 1)
	return nil
}

func (s *threadSafeLockIntentStore) CreateWithDues(ctx context.Context, p *domain.PaymentIntent, dueIDs []uuid.UUID, amounts []int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	atomic.AddInt32(&s.createCalls, 1)
	s.reusable = p
	return nil
}

type countingOrderCreator struct {
	mu    sync.Mutex
	count int32
}

func (c *countingOrderCreator) CreateUPIOrder(ctx context.Context, orderID string, amountPaise int64, customerPhone, note string, idempotencyKey ...string) (string, *time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	atomic.AddInt32(&c.count, 1)
	exp := time.Now().Add(30 * time.Minute)
	return "sess_single_active", &exp, nil
}

func (c *countingOrderCreator) GetOrder(ctx context.Context, orderID string) (*cashfree.OrderDetails, error) {
	return nil, nil
}

// TestConcurrency_TestC_DoubleTapOrderCreation verifies that concurrent calls to PayIntent
// for the same due do not produce duplicate active payment sessions or runaway gateway orders.
func TestConcurrency_TestC_DoubleTapOrderCreation(t *testing.T) {
	store := &threadSafeLockIntentStore{}
	creator := &countingOrderCreator{}
	svc := New(store, creator)

	tenantID := uuid.New()
	dueID := uuid.New()
	due := &domain.Due{
		ID:       dueID,
		TenantID: tenantID,
		DueCode:  "DOUBLETAP1",
		Amount:   550000,
		Status:   domain.DueStatusPending,
	}
	prop := &domain.Property{
		ID:          uuid.New(),
		PaymentMode: domain.PaymentModeCashfree,
		UPIVPA:      "owner@upi",
	}

	const goroutines = 10
	var wg sync.WaitGroup
	wg.Add(goroutines)

	sessions := make([]string, goroutines)
	errors := make([]error, goroutines)

	// Simulate simultaneous clicks / requests hitting the API at once
	startBarrier := make(chan struct{})

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			<-startBarrier
			intent, _, err := svc.PayIntent(context.Background(), due, prop, "101", "/qr", "+919876543210")
			if err != nil {
				errors[idx] = err
				return
			}
			if intent != nil {
				sessions[idx] = intent.PaymentSessionID
			}
		}(i)
	}

	close(startBarrier)
	wg.Wait()

	for idx, err := range errors {
		if err != nil {
			t.Fatalf("goroutine %d encountered error: %v", idx, err)
		}
	}

	firstSession := sessions[0]
	if firstSession != "sess_single_active" {
		t.Fatalf("unexpected session ID: %s", firstSession)
	}
	for i := 1; i < goroutines; i++ {
		if sessions[i] != firstSession {
			t.Fatalf("session mismatch at %d: %s != %s", i, sessions[i], firstSession)
		}
	}

	// Verify that under lock reuse prevented runaway gateway order creation
	if creator.count != 1 {
		t.Fatalf("expected exactly 1 gateway order created, got %d", creator.count)
	}
}

// mockMultiDueIntentManager simulates PostgreSQL uq_active_intent_dues_item enforcement
// and in-flight overlapping due isolation.
type mockMultiDueIntentManager struct {
	mu             sync.Mutex
	activeDues     map[uuid.UUID]uuid.UUID // dueID -> intentID
	intents        map[uuid.UUID]*domain.PaymentIntent
	createAttempts int32
}

func newMockMultiDueIntentManager() *mockMultiDueIntentManager {
	return &mockMultiDueIntentManager{
		activeDues: make(map[uuid.UUID]uuid.UUID),
		intents:    make(map[uuid.UUID]*domain.PaymentIntent),
	}
}

func (m *mockMultiDueIntentManager) Create(context.Context, *domain.PaymentIntent) error {
	return nil
}

func (m *mockMultiDueIntentManager) LatestOpenForDue(context.Context, uuid.UUID) (*domain.PaymentIntent, error) {
	return nil, nil
}

func (m *mockMultiDueIntentManager) ClaimInitiatingIntent(
	ctx context.Context,
	tenantID uuid.UUID,
	dueIDs []uuid.UUID,
	amounts []int64,
	totalAmountPaise int64,
	orderID string,
	minRemaining time.Duration,
) (claimed *domain.PaymentIntent, isReused bool, isInFlight bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if ANY due in the request is already active in another in-flight intent
	for _, dID := range dueIDs {
		if existingIntentID, active := m.activeDues[dID]; active {
			existing := m.intents[existingIntentID]
			// Overlapping due detected - return in-flight intent without permitting new order
			return existing, false, true, nil
		}
	}

	// Claim new initiating intent
	intentID := uuid.New()
	newIntent := &domain.PaymentIntent{
		ID:              intentID,
		ProviderOrderID: orderID,
		AmountPaise:     totalAmountPaise,
		Status:          domain.IntentInitiating,
		IdempotencyKey:  uuid.New(),
	}
	m.intents[intentID] = newIntent
	for _, dID := range dueIDs {
		m.activeDues[dID] = intentID
	}
	return newIntent, false, false, nil
}

func (m *mockMultiDueIntentManager) TransitionCreated(ctx context.Context, intentID uuid.UUID, sessionID string, expiresAt *time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if it, ok := m.intents[intentID]; ok {
		it.Status = domain.IntentCreated
		it.PaymentSessionID = &sessionID
		it.ExpiresAt = expiresAt
	}
	return nil
}

func (m *mockMultiDueIntentManager) TransitionFailed(ctx context.Context, intentID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if it, ok := m.intents[intentID]; ok {
		it.Status = domain.IntentFailed
	}
	return nil
}

func (m *mockMultiDueIntentManager) TransitionDispatchUnknown(ctx context.Context, intentID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if it, ok := m.intents[intentID]; ok {
		it.Status = domain.IntentDispatchUnknown
	}
	return nil
}

func (m *mockMultiDueIntentManager) GetByID(ctx context.Context, id uuid.UUID) (*domain.PaymentIntent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if it, ok := m.intents[id]; ok {
		return it, nil
	}
	return nil, nil
}

// TestConcurrency_MultiDueOverlappingRequestBlocked verifies that two multi-due checkout requests
// sharing an overlapping due (e.g. Request 1 has [Due1, Due2] and Request 2 has [Due2, Due3])
// cannot create conflicting active orders at the provider.
func TestConcurrency_MultiDueOverlappingRequestBlocked(t *testing.T) {
	mgr := newMockMultiDueIntentManager()
	creator := &countingOrderCreator{}
	svc := New(mgr, creator)

	tenantID := uuid.New()
	due1 := &domain.Due{ID: uuid.New(), TenantID: tenantID, DueCode: "DUE1", Amount: 500000, Status: domain.DueStatusPending}
	due2 := &domain.Due{ID: uuid.New(), TenantID: tenantID, DueCode: "DUE2", Amount: 500000, Status: domain.DueStatusPending}
	due3 := &domain.Due{ID: uuid.New(), TenantID: tenantID, DueCode: "DUE3", Amount: 500000, Status: domain.DueStatusPending}

	prop := &domain.Property{
		ID:          uuid.New(),
		PaymentMode: domain.PaymentModeCashfree,
	}

	var wg sync.WaitGroup
	wg.Add(2)

	var session1, session2 string
	var err1, err2 error

	startBarrier := make(chan struct{})

	// Request A: dues [due1, due2]
	go func() {
		defer wg.Done()
		<-startBarrier
		intent, err := svc.MultiDuePayIntent(context.Background(), []*domain.Due{due1, due2}, prop, "101", "+919876543210")
		err1 = err
		if intent != nil {
			session1 = intent.PaymentSessionID
		}
	}()

	// Request B: dues [due2, due3] (overlapping due2)
	go func() {
		defer wg.Done()
		<-startBarrier
		intent, err := svc.MultiDuePayIntent(context.Background(), []*domain.Due{due2, due3}, prop, "101", "+919876543210")
		err2 = err
		if intent != nil {
			session2 = intent.PaymentSessionID
		}
	}()

	close(startBarrier)
	wg.Wait()

	// Exactly one order should have been created at Cashfree for the winning request
	// while the second request was blocked by the overlapping due and awaited reconciliation
	if creator.count != 1 {
		t.Fatalf("expected exactly 1 provider order created, got %d", creator.count)
	}

	// At least one request successfully completed with a session
	if session1 == "" && session2 == "" {
		t.Fatalf("expected at least one request to succeed, err1=%v err2=%v", err1, err2)
	}
}
