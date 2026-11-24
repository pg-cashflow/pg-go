package payment_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

type mockIntentLifecycleStore struct {
	byOrderID       map[string]*domain.PaymentIntent
	byCFID          map[string]*domain.PaymentIntent
	byDueID         map[uuid.UUID]*domain.PaymentIntent
	touchedOrderIDs []string
	paidIntents     map[uuid.UUID]string
	errOnTouch      error
	errOnMarkPaid   error
	errOnLatestOpen error
	errOnGetByOrder error
}

func newMockIntentLifecycleStore() *mockIntentLifecycleStore {
	return &mockIntentLifecycleStore{
		byOrderID:   make(map[string]*domain.PaymentIntent),
		byCFID:      make(map[string]*domain.PaymentIntent),
		byDueID:     make(map[uuid.UUID]*domain.PaymentIntent),
		paidIntents: make(map[uuid.UUID]string),
	}
}

func (m *mockIntentLifecycleStore) GetByOrderID(_ context.Context, orderID string) (*domain.PaymentIntent, error) {
	if m.errOnGetByOrder != nil {
		return nil, m.errOnGetByOrder
	}
	return m.byOrderID[orderID], nil
}

func (m *mockIntentLifecycleStore) GetByCFPaymentID(_ context.Context, cfID string) (*domain.PaymentIntent, error) {
	return m.byCFID[cfID], nil
}

func (m *mockIntentLifecycleStore) MarkPaid(_ context.Context, id uuid.UUID, cfPaymentID string) error {
	if m.errOnMarkPaid != nil {
		return m.errOnMarkPaid
	}
	m.paidIntents[id] = cfPaymentID
	return nil
}

func (m *mockIntentLifecycleStore) LatestOpenForDue(_ context.Context, dueID uuid.UUID) (*domain.PaymentIntent, error) {
	if m.errOnLatestOpen != nil {
		return nil, m.errOnLatestOpen
	}
	return m.byDueID[dueID], nil
}

func (m *mockIntentLifecycleStore) TouchNonTerminalByOrderID(_ context.Context, orderID string) error {
	if m.errOnTouch != nil {
		return m.errOnTouch
	}
	m.touchedOrderIDs = append(m.touchedOrderIDs, orderID)
	return nil
}

func TestIntentLifecycle_RecordAttemptFailed(t *testing.T) {
	ctx := context.Background()

	t.Run("empty order ID returns error", func(t *testing.T) {
		lc := payment.NewIntentLifecycle(newMockIntentLifecycleStore())
		err := lc.RecordAttemptFailed(ctx, "", "cf_pay_1")
		if err == nil {
			t.Fatal("expected error for empty order ID")
		}
	})

	t.Run("nil store returns error", func(t *testing.T) {
		lc := payment.NewIntentLifecycle(nil)
		err := lc.RecordAttemptFailed(ctx, "order_1", "cf_pay_1")
		if err == nil {
			t.Fatal("expected error for nil store")
		}
	})

	t.Run("touches non-terminal on failed attempt without marking terminal", func(t *testing.T) {
		store := newMockIntentLifecycleStore()
		lc := payment.NewIntentLifecycle(store)
		err := lc.RecordAttemptFailed(ctx, "order_123", "cf_attempt_failed_1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(store.touchedOrderIDs) != 1 || store.touchedOrderIDs[0] != "order_123" {
			t.Fatalf("expected order_123 to be touched, got %v", store.touchedOrderIDs)
		}
	})

	t.Run("propagates store error", func(t *testing.T) {
		store := newMockIntentLifecycleStore()
		store.errOnTouch = errors.New("db connection failure")
		lc := payment.NewIntentLifecycle(store)
		err := lc.RecordAttemptFailed(ctx, "order_fail", "cf_1")
		if err == nil || !errors.Is(err, store.errOnTouch) {
			t.Fatalf("expected store error, got %v", err)
		}
	})
}

func TestIntentLifecycle_MarkSettled(t *testing.T) {
	ctx := context.Background()

	t.Run("nil intent ID returns error", func(t *testing.T) {
		lc := payment.NewIntentLifecycle(newMockIntentLifecycleStore())
		err := lc.MarkSettled(ctx, uuid.Nil, "cf_pay_1")
		if err == nil {
			t.Fatal("expected error for nil intent ID")
		}
	})

	t.Run("nil store returns error", func(t *testing.T) {
		lc := payment.NewIntentLifecycle(nil)
		err := lc.MarkSettled(ctx, uuid.New(), "cf_pay_1")
		if err == nil {
			t.Fatal("expected error for nil store")
		}
	})

	t.Run("marks intent paid in store", func(t *testing.T) {
		store := newMockIntentLifecycleStore()
		lc := payment.NewIntentLifecycle(store)
		id := uuid.New()
		err := lc.MarkSettled(ctx, id, "cf_settled_99")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if store.paidIntents[id] != "cf_settled_99" {
			t.Fatalf("expected %s, got %s", "cf_settled_99", store.paidIntents[id])
		}
	})
}

func TestIntentLifecycle_ReusableSessionForDue(t *testing.T) {
	ctx := context.Background()
	dueID := uuid.New()
	sessID := "session_abc"

	t.Run("nil due ID returns error", func(t *testing.T) {
		lc := payment.NewIntentLifecycle(newMockIntentLifecycleStore())
		_, err := lc.ReusableSessionForDue(ctx, uuid.Nil, time.Minute)
		if err == nil {
			t.Fatal("expected error for nil due ID")
		}
	})

	t.Run("nil store returns error", func(t *testing.T) {
		lc := payment.NewIntentLifecycle(nil)
		_, err := lc.ReusableSessionForDue(ctx, dueID, time.Minute)
		if err == nil {
			t.Fatal("expected error for nil store")
		}
	})

	t.Run("no open intent returns nil", func(t *testing.T) {
		store := newMockIntentLifecycleStore()
		lc := payment.NewIntentLifecycle(store)
		intent, err := lc.ReusableSessionForDue(ctx, dueID, time.Minute)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if intent != nil {
			t.Fatalf("expected nil intent, got %+v", intent)
		}
	})

	t.Run("intent with empty session returns nil", func(t *testing.T) {
		store := newMockIntentLifecycleStore()
		empty := ""
		store.byDueID[dueID] = &domain.PaymentIntent{
			ID:               uuid.New(),
			DueID:            dueID,
			PaymentSessionID: &empty,
		}
		lc := payment.NewIntentLifecycle(store)
		intent, err := lc.ReusableSessionForDue(ctx, dueID, time.Minute)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if intent != nil {
			t.Fatalf("expected nil intent, got %+v", intent)
		}
	})

	t.Run("intent expiring sooner than minRemaining returns nil", func(t *testing.T) {
		store := newMockIntentLifecycleStore()
		soon := time.Now().Add(30 * time.Second)
		store.byDueID[dueID] = &domain.PaymentIntent{
			ID:               uuid.New(),
			DueID:            dueID,
			PaymentSessionID: &sessID,
			ExpiresAt:        &soon,
		}
		lc := payment.NewIntentLifecycle(store)
		intent, err := lc.ReusableSessionForDue(ctx, dueID, 2*time.Minute)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if intent != nil {
			t.Fatalf("expected expired intent to not be reusable, got %+v", intent)
		}
	})

	t.Run("intent with adequate remaining lifetime is reusable", func(t *testing.T) {
		store := newMockIntentLifecycleStore()
		later := time.Now().Add(15 * time.Minute)
		expected := &domain.PaymentIntent{
			ID:               uuid.New(),
			DueID:            dueID,
			PaymentSessionID: &sessID,
			ExpiresAt:        &later,
		}
		store.byDueID[dueID] = expected
		lc := payment.NewIntentLifecycle(store)
		intent, err := lc.ReusableSessionForDue(ctx, dueID, 2*time.Minute)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if intent == nil || intent.ID != expected.ID {
			t.Fatalf("expected intent %s, got %+v", expected.ID, intent)
		}
	})
}

func TestIntentLifecycle_VerifyLookups(t *testing.T) {
	ctx := context.Background()
	store := newMockIntentLifecycleStore()
	lc := payment.NewIntentLifecycle(store)

	orderID := "order_verify_test"
	dueID := uuid.New()
	intent := &domain.PaymentIntent{
		ID:              uuid.New(),
		DueID:           dueID,
		ProviderOrderID: orderID,
	}
	store.byOrderID[orderID] = intent
	store.byDueID[dueID] = intent

	t.Run("IntentToVerify success and empty check", func(t *testing.T) {
		_, err := lc.IntentToVerify(ctx, "")
		if err == nil {
			t.Fatal("expected error for empty order ID")
		}
		got, err := lc.IntentToVerify(ctx, orderID)
		if err != nil || got == nil || got.ID != intent.ID {
			t.Fatalf("expected intent %s, got %+v (err=%v)", intent.ID, got, err)
		}
	})

	t.Run("IntentToVerifyForDue success and nil check", func(t *testing.T) {
		_, err := lc.IntentToVerifyForDue(ctx, uuid.Nil)
		if err == nil {
			t.Fatal("expected error for nil due ID")
		}
		got, err := lc.IntentToVerifyForDue(ctx, dueID)
		if err != nil || got == nil || got.ID != intent.ID {
			t.Fatalf("expected intent %s, got %+v (err=%v)", intent.ID, got, err)
		}
	})
}
