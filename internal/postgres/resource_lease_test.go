package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestResourceLeaseManager_NilPoolReturnsError(t *testing.T) {
	manager := NewResourceLeaseManager(nil)

	// TryAcquireLease fails closed
	lease, acquired, err := manager.TryAcquireLease(context.Background(), "job", "test-job", "worker-1", 10*time.Second)
	if err == nil {
		t.Fatalf("expected error on nil pool, got nil")
	}
	if acquired {
		t.Fatalf("expected acquired=false on nil pool")
	}
	if lease != nil {
		t.Fatalf("expected nil lease on nil pool")
	}

	// RenewLease fails closed
	renewed, err := manager.RenewLease(context.Background(), &ResourceLease{
		ResourceType: "job",
		ResourceID:   "test-job",
		LeaseToken:   uuid.New(),
		Generation:   1,
	}, 10*time.Second)
	if err == nil {
		t.Fatalf("expected error on RenewLease with nil pool")
	}
	if renewed {
		t.Fatalf("expected renewed=false on nil pool")
	}

	// ReleaseLease fails closed
	err = manager.ReleaseLease(context.Background(), &ResourceLease{
		ResourceType: "job",
		ResourceID:   "test-job",
		LeaseToken:   uuid.New(),
		Generation:   1,
	})
	if err == nil {
		t.Fatalf("expected error on ReleaseLease with nil pool")
	}
}

func TestResourceLease_Structure(t *testing.T) {
	token := uuid.New()
	expires := time.Now().Add(30 * time.Second)
	lease := &ResourceLease{
		ResourceType: "refund",
		ResourceID:   "ref-123",
		LeaseOwner:   "worker-a",
		LeaseToken:   token,
		Generation:   42,
		ExpiresAt:    expires,
	}

	if lease.ResourceType != "refund" || lease.ResourceID != "ref-123" {
		t.Errorf("unexpected resource identifier: %s, %s", lease.ResourceType, lease.ResourceID)
	}
	if lease.Generation != 42 {
		t.Errorf("expected generation 42, got %d", lease.Generation)
	}
	if lease.LeaseToken != token {
		t.Errorf("expected token %s, got %s", token, lease.LeaseToken)
	}
}
