package notification

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// CompositeRepo implements DispatchRepo by combining the two storage types.
// This is the concrete adapter wired in main.go and used by the Dispatcher.
type CompositeRepo struct {
	OutboxRepo *postgres.OutboxRepo
	NotifRepo  *postgres.NotificationRepo
}

func (c *CompositeRepo) FetchCandidateIDs(ctx context.Context, limit int) ([]int64, error) {
	return c.OutboxRepo.FetchCandidateIDs(ctx, limit)
}

func (c *CompositeRepo) LockAndFetchEventTx(ctx context.Context, tx pgx.Tx, id int64) (*domain.OutboxEvent, error) {
	return c.OutboxRepo.LockAndFetchEventTx(ctx, tx, id)
}

func (c *CompositeRepo) MarkDispatchedTx(ctx context.Context, tx pgx.Tx, id int64) error {
	return c.OutboxRepo.MarkDispatchedTx(ctx, tx, id)
}

func (c *CompositeRepo) RecordEventFailureTx(ctx context.Context, tx pgx.Tx, id int64, newAttempts int, errorJSON string, nextRetryAt time.Time, isDeadLetter bool) error {
	return c.OutboxRepo.RecordEventFailureTx(ctx, tx, id, newAttempts, errorJSON, nextRetryAt, isDeadLetter)
}

func (c *CompositeRepo) CreateOrGetNotificationTx(ctx context.Context, tx pgx.Tx, n *domain.Notification) (uuid.UUID, error) {
	return c.NotifRepo.CreateOrGetNotificationTx(ctx, tx, n)
}

func (c *CompositeRepo) RecordDeliveryTx(ctx context.Context, tx pgx.Tx, d *domain.NotificationDelivery) error {
	return c.NotifRepo.RecordDeliveryTx(ctx, tx, d)
}
