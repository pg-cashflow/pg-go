package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
)

func TestRefundOperationLock_NilPoolFailsClosed(t *testing.T) {
	locker := NewRefundOperationLock(nil)
	refID := uuid.New()

	acquired, unlock, err := locker.TryLockRefundOperation(context.Background(), refID)
	if !errors.Is(err, ErrUnconfiguredPool) {
		t.Fatalf("expected ErrUnconfiguredPool for nil pool, got %v", err)
	}
	if acquired {
		t.Fatalf("expected acquired=false for nil pool")
	}
	if unlock != nil {
		t.Fatalf("expected nil unlock func for failed acquisition")
	}
}

func TestRefundOperationLock_NoopLocker(t *testing.T) {
	locker := NewNoopRefundOperationLock()
	refID := uuid.New()

	acquired, unlock, err := locker.TryLockRefundOperation(context.Background(), refID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !acquired {
		t.Fatalf("expected acquired=true for noop locker")
	}
	if unlock == nil {
		t.Fatalf("expected non-nil unlock func")
	}
	unlock() // Should not panic
}
