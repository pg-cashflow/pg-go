package postgres

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrUnconfiguredPool = errors.New("refund operation lock: database pool is not configured")

// RefundOperationLocker controls mutual exclusion for refund operations
// (dispatch, cancellation, and poller resubmission) using distributed resource leases.
type RefundOperationLocker interface {
	TryLockRefundOperation(ctx context.Context, refundID uuid.UUID) (acquired bool, unlock func(), err error)
}

// RefundOperationLock provides distributed lease locking keyed by refund UUID.
type RefundOperationLock struct {
	manager *ResourceLeaseManager
	owner   string
}

// NewRefundOperationLock creates a new RefundOperationLock.
func NewRefundOperationLock(pool *pgxpool.Pool) *RefundOperationLock {
	hostname, err := os.Hostname()
	if err != nil || hostname == "" {
		hostname = "worker"
	}
	owner := fmt.Sprintf("%s:%d", hostname, os.Getpid())
	return &RefundOperationLock{
		manager: NewResourceLeaseManager(pool),
		owner:   owner,
	}
}

// TryLockRefundOperation attempts to acquire a distributed resource lease for the refund UUID.
func (l *RefundOperationLock) TryLockRefundOperation(ctx context.Context, refundID uuid.UUID) (bool, func(), error) {
	if l == nil || l.manager == nil || l.manager.pool == nil {
		return false, nil, ErrUnconfiguredPool
	}

	lease, acquired, err := l.manager.TryAcquireLease(ctx, "refund", refundID.String(), l.owner, 30*time.Second)
	if err != nil {
		return false, nil, fmt.Errorf("try refund lease lock: %w", err)
	}
	if !acquired {
		return false, func() {}, nil
	}

	unlock := func() {
		relCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = l.manager.ReleaseLease(relCtx, lease)
	}

	return true, unlock, nil
}

// NoopRefundOperationLock provides a safe no-op locker for isolated unit tests.
type NoopRefundOperationLock struct{}

// NewNoopRefundOperationLock creates a no-op locker for testing.
func NewNoopRefundOperationLock() *NoopRefundOperationLock {
	return &NoopRefundOperationLock{}
}

// TryLockRefundOperation always succeeds with a no-op unlock for testing.
func (n *NoopRefundOperationLock) TryLockRefundOperation(ctx context.Context, refundID uuid.UUID) (bool, func(), error) {
	return true, func() {}, nil
}
