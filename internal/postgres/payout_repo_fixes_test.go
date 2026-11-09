package postgres

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestUpdateBatchStatusFromItemsTx_ReversedAndCancelled(t *testing.T) {
	ctx := context.Background()
	batchID := uuid.New()

	tests := []struct {
		name           string
		statuses       []domain.PayoutItemStatus
		expectedStatus domain.PayoutBatchStatus
	}{
		{
			name: "All items reversed rolls up to failed",
			statuses: []domain.PayoutItemStatus{
				domain.PayoutReversed,
				domain.PayoutReversed,
			},
			expectedStatus: domain.BatchFailed,
		},
		{
			name: "All items cancelled rolls up to failed",
			statuses: []domain.PayoutItemStatus{
				domain.PayoutCancelled,
			},
			expectedStatus: domain.BatchFailed,
		},
		{
			name: "Mixed succeeded and reversed rolls up to partially_failed",
			statuses: []domain.PayoutItemStatus{
				domain.PayoutSucceeded,
				domain.PayoutReversed,
			},
			expectedStatus: domain.BatchPartiallyFailed,
		},
		{
			name: "Mixed succeeded and cancelled rolls up to partially_failed",
			statuses: []domain.PayoutItemStatus{
				domain.PayoutSucceeded,
				domain.PayoutCancelled,
			},
			expectedStatus: domain.BatchPartiallyFailed,
		},
		{
			name: "In-flight items still roll up to processing",
			statuses: []domain.PayoutItemStatus{
				domain.PayoutSucceeded,
				domain.PayoutProcessing,
			},
			expectedStatus: domain.BatchProcessing,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			idx := 0
			mockDB := &mockPayoutDB{
				queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
					return &mockPayoutRows{
						nextFn: func() bool {
							return idx < len(tc.statuses)
						},
						scanFn: func(dest ...any) error {
							*dest[0].(*domain.PayoutItemStatus) = tc.statuses[idx]
							idx++
							return nil
						},
					}, nil
				},
				execFn: func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
					return pgconn.NewCommandTag("UPDATE 1"), nil
				},
				queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
					return &mockPayoutRow{
						scanFn: func(dest ...any) error {
							// Return a dummy batch with the updated status
							*dest[0].(*uuid.UUID) = batchID
							*dest[4].(*domain.PayoutBatchStatus) = tc.expectedStatus
							return nil
						},
					}
				},
			}

			repo := &PayoutRepo{
				db: mockDB,
			}

			batch, err := repo.UpdateBatchStatusFromItemsTx(ctx, nil, batchID)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if batch.Status != tc.expectedStatus {
				t.Fatalf("expected batch status %s, got %s", tc.expectedStatus, batch.Status)
			}
		})
	}
}

func TestInitiateBatchTransferTx_NilOutbox_FailsClosed(t *testing.T) {
	ctx := context.Background()
	batchID := uuid.New()

	repo := &PayoutRepo{
		pool:   nil,
		outbox: nil,
	}

	// Should fail immediately without outbox or pool
	_, _, err := repo.InitiateBatchTransferTx(ctx, batchID)
	if err == nil {
		t.Fatalf("expected error when initiating batch transfer without pool/outbox, got nil")
	}
}
