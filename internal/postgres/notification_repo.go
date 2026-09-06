package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// NotificationRepo manages notifications and notification_deliveries.
type NotificationRepo struct {
	db   DBTX
	pool *pgxpool.Pool // for unread count (separate query outside caller tx)
}

func NewNotificationRepo(pool *pgxpool.Pool) *NotificationRepo {
	return &NotificationRepo{db: pool, pool: pool}
}

// CreateOrGetNotificationTx inserts a notification row inside the caller's
// transaction (or savepoint). Uses ON CONFLICT DO NOTHING so retries after a
// crash are safe — if the row already exists it fetches and returns the
// existing id instead, so the delivery bookkeeping row can be correctly linked.
func (r *NotificationRepo) CreateOrGetNotificationTx(ctx context.Context, tx pgx.Tx, n *domain.Notification) (uuid.UUID, error) {
	if n.ID == uuid.Nil {
		n.ID = uuid.New()
	}
	if n.CreatedAt.IsZero() {
		n.CreatedAt = time.Now().UTC()
	}

	var id uuid.UUID
	err := tx.QueryRow(ctx, `
		INSERT INTO notifications
			(id, source_event_id, recipient_id, property_id, type, title, deep_link, is_action_required, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		ON CONFLICT (source_event_id, recipient_id) DO NOTHING
		RETURNING id`,
		n.ID, n.SourceEventID, n.RecipientID, n.PropertyID,
		n.Type, n.Title, n.DeepLink, n.IsActionRequired, n.CreatedAt,
	).Scan(&id)

	if err == pgx.ErrNoRows {
		// Conflict: row already exists from a prior (crashed) attempt.
		// Fetch the existing id so delivery bookkeeping can be linked correctly.
		err = tx.QueryRow(ctx, `
			SELECT id FROM notifications
			WHERE source_event_id = $1 AND recipient_id = $2`,
			n.SourceEventID, n.RecipientID,
		).Scan(&id)
	}
	return id, err
}

// RecordDeliveryTx inserts a delivery attempt row inside the caller's
// transaction (or savepoint). ON CONFLICT DO NOTHING makes this idempotent
// across retries; the unique constraint is on (notification_id, channel).
func (r *NotificationRepo) RecordDeliveryTx(ctx context.Context, tx pgx.Tx, d *domain.NotificationDelivery) error {
	if d.ID == uuid.Nil {
		d.ID = uuid.New()
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO notification_deliveries
			(id, notification_id, channel, status, attempt_count, last_error, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (notification_id, channel) DO NOTHING`,
		d.ID, d.NotificationID, d.Channel, d.Status, d.AttemptCount, d.LastError,
	)
	return err
}

// ListNotifications returns paginated notifications for a recipient using a
// compound (created_at DESC, id DESC) cursor to prevent duplicate or skipped
// items when multiple notifications share identical timestamps.
// Also returns the total unread count as a cheap sibling query.
func (r *NotificationRepo) ListNotifications(
	ctx context.Context,
	recipientID uuid.UUID,
	cursor *domain.NotificationCursor,
	limit int,
) ([]*domain.Notification, *domain.NotificationCursor, int, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}

	var rows pgx.Rows
	var err error
	if cursor == nil {
		rows, err = r.db.Query(ctx, `
			SELECT id, source_event_id, recipient_id, property_id, type, title, deep_link,
			       is_action_required, read_at, created_at
			FROM notifications
			WHERE recipient_id = $1
			ORDER BY created_at DESC, id DESC
			LIMIT $2`,
			recipientID, limit,
		)
	} else {
		rows, err = r.db.Query(ctx, `
			SELECT id, source_event_id, recipient_id, property_id, type, title, deep_link,
			       is_action_required, read_at, created_at
			FROM notifications
			WHERE recipient_id = $1
			  AND (created_at, id) < ($2, $3)
			ORDER BY created_at DESC, id DESC
			LIMIT $4`,
			recipientID, cursor.CreatedAt, cursor.ID, limit,
		)
	}
	if err != nil {
		return nil, nil, 0, err
	}
	defer rows.Close()

	var out []*domain.Notification
	for rows.Next() {
		n := &domain.Notification{}
		if err := rows.Scan(
			&n.ID, &n.SourceEventID, &n.RecipientID, &n.PropertyID,
			&n.Type, &n.Title, &n.DeepLink,
			&n.IsActionRequired, &n.ReadAt, &n.CreatedAt,
		); err != nil {
			return nil, nil, 0, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, 0, err
	}

	var nextCursor *domain.NotificationCursor
	if len(out) == limit {
		last := out[len(out)-1]
		nextCursor = &domain.NotificationCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}

	var unread int
	_ = r.db.QueryRow(ctx, `
		SELECT COUNT(*) FROM notifications
		WHERE recipient_id = $1 AND read_at IS NULL`, recipientID,
	).Scan(&unread)

	return out, nextCursor, unread, nil
}

// MarkAsRead marks a single notification as read. The recipient_id scope
// ensures tenants can't mark other recipients' notifications.
func (r *NotificationRepo) MarkAsRead(ctx context.Context, notificationID, recipientID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		UPDATE notifications SET read_at = now()
		WHERE id = $1 AND recipient_id = $2 AND read_at IS NULL`,
		notificationID, recipientID,
	)
	return err
}

// MarkAllAsRead marks every unread notification for a recipient as read.
func (r *NotificationRepo) MarkAllAsRead(ctx context.Context, recipientID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		UPDATE notifications SET read_at = now()
		WHERE recipient_id = $1 AND read_at IS NULL`, recipientID,
	)
	return err
}
