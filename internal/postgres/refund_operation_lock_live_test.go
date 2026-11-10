package postgres

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

func TestRefundOperationLock_NilPoolNoOp(t *testing.T) {
	locker := NewRefundOperationLock(nil)
	refID := uuid.New()

	acquired, unlock, err := locker.TryLockRefundOperation(context.Background(), refID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !acquired {
		t.Fatalf("expected acquired=true for nil pool")
	}
	if unlock == nil {
		t.Fatalf("expected non-nil unlock func")
	}
	unlock() // Should not panic
}
