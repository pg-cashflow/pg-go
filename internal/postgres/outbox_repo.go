package postgres

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// OutboxRepo manages the outbox_events table.
//
// Locking contract:
//   - FetchCandidateIDs is a lock-free read — callers iterate candidate IDs.
//   - LockAndFetchEventTx uses FOR UPDATE SKIP LOCKED inside a caller-owned
//     transaction, holding the row lock for the event's entire processing window.
//   - MarkDispatchedTx and RecordEventFailureTx both execute within that same
//     caller-owned transaction, so the lock is never released until the outcome
//     (dispatched or failure+backoff) is committed to disk.
type OutboxRepo struct{ db DBTX }

func NewOutboxRepo(db DBTX) *OutboxRepo { return &OutboxRepo{db: db} }

// InsertOutboxEventTx writes a new outbox event within an already-open
// transaction (the same txn as the triggering domain write). Payload is
// IDs-only — never raw money strings or PII.
func (r *OutboxRepo) InsertOutboxEventTx(ctx context.Context, tx pgx.Tx, evt *domain.OutboxEvent) error {
	if evt.Payload == nil {
		evt.Payload = json.RawMessage("{}")
	}
	return tx.QueryRow(ctx, `
		INSERT INTO outbox_events (event_type, property_id, tenant_id, actor_role, payload)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at`,
		evt.EventType, evt.PropertyID, evt.TenantID, evt.ActorRole, evt.Payload,
	).Scan(&evt.ID, &evt.CreatedAt)
}

// InsertEvent writes a new outbox event using a standalone connection from the
// pool. Use this from handlers that do not expose a shared transaction boundary
// (same best-effort pattern as the existing h.Events.Publish audit log write).
// If this insert fails the notification is simply skipped — the audit event is
// already committed and domain consistency is preserved.
func (r *OutboxRepo) InsertEvent(ctx context.Context, evt *domain.OutboxEvent) error {
	if evt.Payload == nil {
		evt.Payload = json.RawMessage("{}")
	}
	return r.db.QueryRow(ctx, `
		INSERT INTO outbox_events (event_type, property_id, tenant_id, actor_role, payload)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, created_at`,
		evt.EventType, evt.PropertyID, evt.TenantID, evt.ActorRole, evt.Payload,
	).Scan(&evt.ID, &evt.CreatedAt)
}

// FetchCandidateIDs returns IDs of events eligible for dispatch — no row locks
// held. The caller re-locks each row individually in LockAndFetchEventTx.
func (r *OutboxRepo) FetchCandidateIDs(ctx context.Context, limit int) ([]int64, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id FROM outbox_events
		WHERE dispatched_at IS NULL
		  AND failed_at     IS NULL
		  AND (next_retry_at IS NULL OR next_retry_at <= now())
		ORDER BY id ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// LockAndFetchEventTx locks the row with FOR UPDATE SKIP LOCKED inside the
// caller's transaction. Returns nil, nil if the row is already locked by
// another worker (SKIP LOCKED) or has been dispatched/failed since candidate
// fetch — the caller should skip and move to the next candidate.
func (r *OutboxRepo) LockAndFetchEventTx(ctx context.Context, tx pgx.Tx, id int64) (*domain.OutboxEvent, error) {
	var evt domain.OutboxEvent
	var lastError *string
	var nextRetryAt *time.Time
	var dispatchedAt *time.Time
	var failedAt *time.Time

	err := tx.QueryRow(ctx, `
		SELECT id, event_type, property_id, tenant_id, actor_role, payload,
		       attempt_count, last_error, next_retry_at, created_at, dispatched_at, failed_at
		FROM outbox_events
		WHERE id = $1
		  AND dispatched_at IS NULL
		  AND failed_at     IS NULL
		FOR UPDATE SKIP LOCKED`, id,
	).Scan(
		&evt.ID, &evt.EventType, &evt.PropertyID, &evt.TenantID, &evt.ActorRole, &evt.Payload,
		&evt.AttemptCount, &lastError, &nextRetryAt, &evt.CreatedAt, &dispatchedAt, &failedAt,
	)
	if err == pgx.ErrNoRows {
		return nil, nil // row skipped or already processed
	}
	if err != nil {
		return nil, err
	}
	evt.LastError = lastError
	evt.NextRetryAt = nextRetryAt
	evt.DispatchedAt = dispatchedAt
	evt.FailedAt = failedAt
	return &evt, nil
}

// MarkDispatchedTx stamps the event as fully dispatched within the caller's
// transaction. The row lock is released only when this transaction commits.
func (r *OutboxRepo) MarkDispatchedTx(ctx context.Context, tx pgx.Tx, id int64) error {
	_, err := tx.Exec(ctx,
		`UPDATE outbox_events SET dispatched_at = now(), next_retry_at = NULL WHERE id = $1`, id)
	return err
}

// RecordEventFailureTx records a failed dispatch attempt within the caller's
// transaction (the same one holding the FOR UPDATE lock), so no concurrent
// worker can grab the row and race the attempt_count increment.
//
// newAttempts and nextRetryAt are pre-computed in Go application code to avoid
// Postgres multi-column SET pre-update read ordering issues.
func (r *OutboxRepo) RecordEventFailureTx(
	ctx context.Context,
	tx pgx.Tx,
	id int64,
	newAttempts int,
	errorJSON string,
	nextRetryAt time.Time,
	isDeadLetter bool,
) error {
	var failedAt *time.Time
	if isDeadLetter {
		t := time.Now().UTC()
		failedAt = &t
	}
	_, err := tx.Exec(ctx, `
		UPDATE outbox_events
		SET attempt_count = $2,
		    last_error    = $3,
		    next_retry_at = $4,
		    failed_at     = $5
		WHERE id = $1`,
		id, newAttempts, errorJSON, nextRetryAt.UTC(), failedAt,
	)
	return err
}

// CleanupEvents prunes old dispatched and dead-lettered outbox events.
// Dispatched events are removed after dispatchedOlderThan (typically 30 days).
// Dead-lettered events are kept longer — failedOlderThan (typically 90 days) —
// to preserve diagnostic visibility.
// Returns the total count of deleted rows.
func (r *OutboxRepo) CleanupEvents(ctx context.Context, dispatchedOlderThan, failedOlderThan time.Duration) (int64, error) {
	tag1, err := r.db.Exec(ctx, `
		DELETE FROM outbox_events
		WHERE dispatched_at IS NOT NULL
		  AND dispatched_at < now() - $1::interval`,
		dispatchedOlderThan.String())
	if err != nil {
		return 0, err
	}
	tag2, err := r.db.Exec(ctx, `
		DELETE FROM outbox_events
		WHERE failed_at IS NOT NULL
		  AND failed_at < now() - $1::interval`,
		failedOlderThan.String())
	if err != nil {
		return tag1.RowsAffected(), err
	}
	return tag1.RowsAffected() + tag2.RowsAffected(), nil
}
