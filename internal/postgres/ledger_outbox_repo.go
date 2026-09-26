package postgres

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// LedgerOutboxRepo manages the ledger_outbox_events table for resilient financial journal posting.
type LedgerOutboxRepo struct {
	db DBTX
}

func NewLedgerOutboxRepo(db DBTX) *LedgerOutboxRepo {
	return &LedgerOutboxRepo{db: db}
}

// InsertLedgerOutboxEventTx inserts an outbox event within the domain write's transaction.
func (r *LedgerOutboxRepo) InsertLedgerOutboxEventTx(ctx context.Context, tx pgx.Tx, evt *domain.LedgerOutboxEvent) error {
	if evt.Payload == nil {
		evt.Payload = json.RawMessage("{}")
	}
	if evt.MaxAttempts <= 0 {
		evt.MaxAttempts = 5
	}
	return tx.QueryRow(ctx, `
		INSERT INTO ledger_outbox_events (
			event_type, property_id, source_id, payload, idempotency_key, max_attempts
		) VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (idempotency_key) DO UPDATE
			SET payload = EXCLUDED.payload
		RETURNING id, created_at`,
		evt.EventType, evt.PropertyID, evt.SourceID, evt.Payload, evt.IdempotencyKey, evt.MaxAttempts,
	).Scan(&evt.ID, &evt.CreatedAt)
}

// FetchPendingCandidateIDs returns IDs of pending ledger outbox events eligible for dispatch.
func (r *LedgerOutboxRepo) FetchPendingCandidateIDs(ctx context.Context, limit int) ([]int64, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id FROM ledger_outbox_events
		WHERE processed_at IS NULL
		  AND failed_at IS NULL
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

// LockAndFetchEventTx locks the event row with FOR UPDATE SKIP LOCKED inside the worker's transaction.
func (r *LedgerOutboxRepo) LockAndFetchEventTx(ctx context.Context, tx pgx.Tx, id int64) (*domain.LedgerOutboxEvent, error) {
	var evt domain.LedgerOutboxEvent
	var lastError *string
	var nextRetryAt *time.Time
	var processedAt *time.Time
	var failedAt *time.Time

	err := tx.QueryRow(ctx, `
		SELECT id, event_type, property_id, source_id, payload, idempotency_key,
		       attempt_count, max_attempts, last_error, next_retry_at, created_at, processed_at, failed_at
		FROM ledger_outbox_events
		WHERE id = $1
		  AND processed_at IS NULL
		  AND failed_at IS NULL
		FOR UPDATE SKIP LOCKED`, id,
	).Scan(
		&evt.ID, &evt.EventType, &evt.PropertyID, &evt.SourceID, &evt.Payload, &evt.IdempotencyKey,
		&evt.AttemptCount, &evt.MaxAttempts, &lastError, &nextRetryAt, &evt.CreatedAt, &processedAt, &failedAt,
	)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, nil // Locked by concurrent worker or already processed
		}
		return nil, err
	}
	evt.LastError = lastError
	evt.NextRetryAt = nextRetryAt
	evt.ProcessedAt = processedAt
	evt.FailedAt = failedAt
	return &evt, nil
}

// MarkProcessedTx marks an outbox event as successfully processed inside the worker transaction.
func (r *LedgerOutboxRepo) MarkProcessedTx(ctx context.Context, tx pgx.Tx, id int64) error {
	_, err := tx.Exec(ctx, `
		UPDATE ledger_outbox_events
		SET processed_at = now()
		WHERE id = $1`, id)
	return err
}

// MarkProcessedByIdempotencyKey marks an event as processed by idempotency key (used for inline fast-path).
func (r *LedgerOutboxRepo) MarkProcessedByIdempotencyKey(ctx context.Context, key string) error {
	_, err := r.db.Exec(ctx, `
		UPDATE ledger_outbox_events
		SET processed_at = now()
		WHERE idempotency_key = $1 AND processed_at IS NULL`, key)
	return err
}

// RecordEventFailureTx updates attempt count, backoff, and escalates if max attempts are reached.
func (r *LedgerOutboxRepo) RecordEventFailureTx(
	ctx context.Context,
	tx pgx.Tx,
	evt *domain.LedgerOutboxEvent,
	newAttempts int,
	errorMsg string,
	nextRetryAt *time.Time,
	isDeadLetter bool,
) error {
	if isDeadLetter {
		// Loud escalation: Never silently abandon a financial ledger outbox event!
		slog.Error("CRITICAL: ledger outbox event exceeded max attempts - financial books desynchronized",
			"event_id", evt.ID,
			"event_type", evt.EventType,
			"source_id", evt.SourceID,
			"idempotency_key", evt.IdempotencyKey,
			"attempts", newAttempts,
			"error", errorMsg,
		)
		_, err := tx.Exec(ctx, `
			UPDATE ledger_outbox_events
			SET attempt_count = $2,
			    last_error    = $3,
			    failed_at     = now(),
			    next_retry_at = NULL
			WHERE id = $1`,
			evt.ID, newAttempts, errorMsg)
		return err
	}

	_, err := tx.Exec(ctx, `
		UPDATE ledger_outbox_events
		SET attempt_count = $2,
		    last_error    = $3,
		    next_retry_at = $4
		WHERE id = $1`,
		evt.ID, newAttempts, errorMsg, nextRetryAt)
	return err
}
