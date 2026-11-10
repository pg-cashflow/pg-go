package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RefundOperationLocker controls mutual exclusion for refund operations
// (dispatch, cancellation, and poller resubmission) using PostgreSQL session advisory locks.
type RefundOperationLocker interface {
	TryLockRefundOperation(ctx context.Context, refundID uuid.UUID) (acquired bool, unlock func(), err error)
}

// RefundOperationLock provides PostgreSQL session advisory locking keyed by refund UUID.
type RefundOperationLock struct {
	pool *pgxpool.Pool
}

// NewRefundOperationLock creates a new RefundOperationLock.
func NewRefundOperationLock(pool *pgxpool.Pool) *RefundOperationLock {
	return &RefundOperationLock{pool: pool}
}

// TryLockRefundOperation attempts to acquire a session-level advisory lock for the refund UUID.
// It acquires a dedicated connection from the pool and releases it upon unlock.
func (l *RefundOperationLock) TryLockRefundOperation(ctx context.Context, refundID uuid.UUID) (bool, func(), error) {
	if l == nil || l.pool == nil {
		// In tests without database pool, allow execution with no-op unlock.
		return true, func() {}, nil
	}

	conn, err := l.pool.Acquire(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("acquire conn for refund operation lock: %w", err)
	}

	var acquired bool
	err = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtext('refund_op:' || $1::text))`, refundID).Scan(&acquired)
	if err != nil {
		conn.Release()
		return false, nil, fmt.Errorf("try refund operation advisory lock: %w", err)
	}

	if !acquired {
		conn.Release()
		return false, func() {}, nil
	}

	unlock := func() {
		defer conn.Release()
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('refund_op:' || $1::text))`, refundID)
	}

	return true, unlock, nil
}
