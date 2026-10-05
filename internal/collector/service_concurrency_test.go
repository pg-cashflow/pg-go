package collector

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
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

func (c *countingOrderCreator) CreateUPIOrder(ctx context.Context, orderID string, amountPaise int64, customerPhone, note string) (string, *time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	atomic.AddInt32(&c.count, 1)
	exp := time.Now().Add(30 * time.Minute)
	return "sess_single_active", &exp, nil
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
