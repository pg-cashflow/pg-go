package payment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// IntentLifecycleStore defines the data access operations required by IntentLifecycle.
type IntentLifecycleStore interface {
	GetByOrderID(ctx context.Context, orderID string) (*domain.PaymentIntent, error)
	GetByCFPaymentID(ctx context.Context, cfID string) (*domain.PaymentIntent, error)
	MarkPaid(ctx context.Context, id uuid.UUID, cfPaymentID string) error
	LatestOpenForDue(ctx context.Context, dueID uuid.UUID) (*domain.PaymentIntent, error)
	TouchNonTerminalByOrderID(ctx context.Context, orderID string) error
}

// IntentLifecycle provides domain operations for managing payment intent states,
// attempt-failure tracking, settlement updates, and verification lookups.
type IntentLifecycle interface {
	RecordAttemptFailed(ctx context.Context, orderID, cfPaymentID string) error
	MarkSettled(ctx context.Context, intentID uuid.UUID, cfPaymentID string) error
	ReusableSessionForDue(ctx context.Context, dueID uuid.UUID, minRemaining time.Duration) (*domain.PaymentIntent, error)
	IntentToVerify(ctx context.Context, orderID string) (*domain.PaymentIntent, error)
	IntentToVerifyForDue(ctx context.Context, dueID uuid.UUID) (*domain.PaymentIntent, error)
}

// DefaultIntentLifecycle implements IntentLifecycle.
type DefaultIntentLifecycle struct {
	store IntentLifecycleStore
}

// NewIntentLifecycle constructs an IntentLifecycle backed by the given store.
func NewIntentLifecycle(store IntentLifecycleStore) *DefaultIntentLifecycle {
	return &DefaultIntentLifecycle{store: store}
}

// RecordAttemptFailed records a non-terminal failed payment attempt on an order.
// Crucially, it does NOT mark the order intent as failed, allowing the payer to retry
// and allowing verify endpoints to look up the intent until order expiry.
func (l *DefaultIntentLifecycle) RecordAttemptFailed(ctx context.Context, orderID, cfPaymentID string) error {
	if orderID == "" {
		return errors.New("payment: orderID required")
	}
	if l.store == nil {
		return errors.New("payment: intent store unavailable")
	}
	if err := l.store.TouchNonTerminalByOrderID(ctx, orderID); err != nil {
		return fmt.Errorf("payment: record attempt failed for %s: %w", orderID, err)
	}
	return nil
}

// MarkSettled marks a payment intent as settled/paid.
func (l *DefaultIntentLifecycle) MarkSettled(ctx context.Context, intentID uuid.UUID, cfPaymentID string) error {
	if intentID == uuid.Nil {
		return errors.New("payment: intentID required")
	}
	if l.store == nil {
		return errors.New("payment: intent store unavailable")
	}
	if err := l.store.MarkPaid(ctx, intentID, cfPaymentID); err != nil {
		return fmt.Errorf("payment: mark intent settled %s: %w", intentID, err)
	}
	return nil
}

// ReusableSessionForDue checks whether there is an active, unexpired payment session
// for the given due with at least minRemaining time before expiration.
func (l *DefaultIntentLifecycle) ReusableSessionForDue(ctx context.Context, dueID uuid.UUID, minRemaining time.Duration) (*domain.PaymentIntent, error) {
	if dueID == uuid.Nil {
		return nil, errors.New("payment: dueID required")
	}
	if l.store == nil {
		return nil, errors.New("payment: intent store unavailable")
	}
	intent, err := l.store.LatestOpenForDue(ctx, dueID)
	if err != nil {
		return nil, err
	}
	if intent == nil || intent.PaymentSessionID == nil || *intent.PaymentSessionID == "" {
		return nil, nil
	}
	if intent.ExpiresAt != nil && time.Until(*intent.ExpiresAt) < minRemaining {
		return nil, nil
	}
	return intent, nil
}

// IntentToVerify resolves the payment intent for an order ID to support payment verification.
func (l *DefaultIntentLifecycle) IntentToVerify(ctx context.Context, orderID string) (*domain.PaymentIntent, error) {
	if orderID == "" {
		return nil, errors.New("payment: orderID required")
	}
	if l.store == nil {
		return nil, errors.New("payment: intent store unavailable")
	}
	return l.store.GetByOrderID(ctx, orderID)
}

// IntentToVerifyForDue resolves the latest open intent for a due to support due payment verification.
func (l *DefaultIntentLifecycle) IntentToVerifyForDue(ctx context.Context, dueID uuid.UUID) (*domain.PaymentIntent, error) {
	if dueID == uuid.Nil {
		return nil, errors.New("payment: dueID required")
	}
	if l.store == nil {
		return nil, errors.New("payment: intent store unavailable")
	}
	return l.store.LatestOpenForDue(ctx, dueID)
}
