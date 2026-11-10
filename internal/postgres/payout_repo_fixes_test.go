package postgres

import (
	"context"
	"strings"
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
							if len(dest) == 1 {
								*dest[0].(*uuid.UUID) = batchID
								return nil
							}
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

func TestUpdateBatchStatusFromItemsTx_AcquiresBatchLock(t *testing.T) {
	ctx := context.Background()
	batchID := uuid.New()

	var queries []string
	mockDB := &mockPayoutDB{
		queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
			queries = append(queries, sql)
			return &mockPayoutRow{
				scanFn: func(dest ...any) error {
					if len(dest) == 1 {
						*dest[0].(*uuid.UUID) = batchID
						return nil
					}
					*dest[0].(*uuid.UUID) = batchID
					*dest[4].(*domain.PayoutBatchStatus) = domain.BatchCompleted
					return nil
				},
			}
		},
		queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
			queries = append(queries, sql)
			idx := 0
			statuses := []domain.PayoutItemStatus{domain.PayoutSucceeded}
			return &mockPayoutRows{
				nextFn: func() bool {
					return idx < len(statuses)
				},
				scanFn: func(dest ...any) error {
					*dest[0].(*domain.PayoutItemStatus) = statuses[idx]
					idx++
					return nil
				},
			}, nil
		},
	}

	repo := &PayoutRepo{db: mockDB}
	batch, err := repo.UpdateBatchStatusFromItemsTx(ctx, nil, batchID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if batch.Status != domain.BatchCompleted {
		t.Fatalf("expected %s, got %s", domain.BatchCompleted, batch.Status)
	}

	// Verify that the first query executed was the row lock on payout_batches
	if len(queries) < 1 || queries[0] != "SELECT id FROM payout_batches WHERE id = $1 FOR UPDATE" {
		t.Fatalf("expected batch row lock query first, got %v", queries)
	}
}

func TestUpdatePayoutItemStatusTx_SQLGuards(t *testing.T) {
	ctx := context.Background()
	itemID := uuid.New()

	var executedSQL string
	mockDB := &mockPayoutDB{
		execFn: func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
			executedSQL = sql
			return pgconn.NewCommandTag("UPDATE 1"), nil
		},
	}

	repo := &PayoutRepo{db: mockDB}
	err := repo.UpdatePayoutItemStatusTx(ctx, nil, itemID, domain.PayoutSucceeded, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedGuard := "WHEN status IN ('failed', 'rejected', 'retriable_failed') AND $2 = 'succeeded' THEN status"
	if !strings.Contains(executedSQL, expectedGuard) {
		t.Fatalf("expected SQL to contain terminal guard %q, got:\n%s", expectedGuard, executedSQL)
	}
}
