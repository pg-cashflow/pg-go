package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestFencedMutations_RejectNilOrCancelledLease(t *testing.T) {
	repo := NewPayoutRepo(nil)
	batchID := uuid.New()
	propID := uuid.New()
	itemID := uuid.New()

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	staleLease := &PayoutDispatchLease{
		BatchID:    batchID,
		Generation: 1,
		Lease: &ResourceLease{
			ResourceType: "payout_batch",
			ResourceID:   batchID.String(),
			LeaseOwner:   "payout-dispatcher",
			LeaseToken:   uuid.New(),
			Generation:   1,
		},
		Ctx: cancelledCtx,
	}

	cases := []struct {
		name  string
		lease *PayoutDispatchLease
	}{
		{name: "nil lease", lease: nil},
		{name: "cancelled lease context", lease: staleLease},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := repo.SetBatchDispatchUnknownFenced(context.Background(), tc.lease, "timeout"); !errors.Is(err, domain.ErrStaleDispatchLease) {
				t.Fatalf("SetBatchDispatchUnknownFenced: expected ErrStaleDispatchLease, got %v", err)
			}
			if err := repo.UpdatePayoutItemStatusFenced(context.Background(), tc.lease, itemID, domain.PayoutFailed, nil, nil, nil, nil); !errors.Is(err, domain.ErrStaleDispatchLease) {
				t.Fatalf("UpdatePayoutItemStatusFenced: expected ErrStaleDispatchLease, got %v", err)
			}
			if err := repo.ReconcilePayoutItemWithOutboxFenced(context.Background(), tc.lease, propID, itemID, 1000, domain.PayoutSucceeded, nil, nil, nil, nil); !errors.Is(err, domain.ErrStaleDispatchLease) {
				t.Fatalf("ReconcilePayoutItemWithOutboxFenced: expected ErrStaleDispatchLease, got %v", err)
			}
			if _, err := repo.UpdateBatchStatusFromItemsFenced(context.Background(), tc.lease); !errors.Is(err, domain.ErrStaleDispatchLease) {
				t.Fatalf("UpdateBatchStatusFromItemsFenced: expected ErrStaleDispatchLease, got %v", err)
			}
			if err := repo.RecordBatchIncidentFenced(context.Background(), tc.lease, "UNKNOWN_TRANSFER_ID", "details"); !errors.Is(err, domain.ErrStaleDispatchLease) {
				t.Fatalf("RecordBatchIncidentFenced: expected ErrStaleDispatchLease, got %v", err)
			}
			if err := repo.RecordItemIncidentFenced(context.Background(), tc.lease, itemID, "TRANSFER_ID_MISMATCH", "exp", "got", "details"); !errors.Is(err, domain.ErrStaleDispatchLease) {
				t.Fatalf("RecordItemIncidentFenced: expected ErrStaleDispatchLease, got %v", err)
			}
		})
	}
}

func TestMaxPayoutBatchItems_Constant(t *testing.T) {
	if domain.MaxPayoutBatchItems != 500 {
		t.Fatalf("expected MaxPayoutBatchItems=500, got %d", domain.MaxPayoutBatchItems)
	}
}
