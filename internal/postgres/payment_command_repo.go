package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// PaymentCommandRepo manages the payment_commands table for durable gateway network requests.
type PaymentCommandRepo struct {
	db DBTX
}

func NewPaymentCommandRepo(db DBTX) *PaymentCommandRepo {
	return &PaymentCommandRepo{db: db}
}

// InsertPaymentCommandTx inserts a payment command within a database transaction.
func (r *PaymentCommandRepo) InsertPaymentCommandTx(ctx context.Context, tx pgx.Tx, cmd *domain.PaymentCommand) error {
	if cmd.Payload == nil {
		cmd.Payload = json.RawMessage("{}")
	}
	if cmd.MaxAttempts <= 0 {
		cmd.MaxAttempts = domain.DefaultPaymentCommandMaxAttempts
	}
	if cmd.Status == "" {
		cmd.Status = domain.PaymentCommandStatusPending
	}

	var (
		existingID      int64
		existingPayload []byte
		existingCreated time.Time
	)
	err := tx.QueryRow(ctx, `
		SELECT id, payload, created_at FROM payment_commands
		WHERE idempotency_key = $1`, cmd.IdempotencyKey,
	).Scan(&existingID, &existingPayload, &existingCreated)

	if err == nil {
		if !jsonPayloadsMatch(existingPayload, cmd.Payload) {
			return fmt.Errorf("%w: idempotency key '%s' already exists with conflicting payload", domain.ErrIdempotencyConflict, cmd.IdempotencyKey)
		}
		cmd.ID = existingID
		cmd.CreatedAt = existingCreated
		return nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}

	return tx.QueryRow(ctx, `
		INSERT INTO payment_commands (
			command_type, property_id, source_id, payload, idempotency_key, status, max_attempts
		) VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, created_at`,
		cmd.CommandType, cmd.PropertyID, cmd.SourceID, cmd.Payload, cmd.IdempotencyKey, cmd.Status, cmd.MaxAttempts,
	).Scan(&cmd.ID, &cmd.CreatedAt)
}

// ListPendingCommandIDs returns IDs of pending payment commands that are due for execution.
func (r *PaymentCommandRepo) ListPendingCommandIDs(ctx context.Context, limit int) ([]int64, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.Query(ctx, `
		SELECT id FROM payment_commands
		WHERE status IN ('pending', 'processing')
		  AND next_retry_at <= now()
		  AND (locked_until IS NULL OR locked_until <= now())
		ORDER BY next_retry_at ASC, id ASC
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

// LockAndClaimCommandTx claims an individual payment command using FOR UPDATE SKIP LOCKED.
func (r *PaymentCommandRepo) LockAndClaimCommandTx(ctx context.Context, tx pgx.Tx, id int64, leaseDuration time.Duration) (*domain.PaymentCommand, error) {
	var cmd domain.PaymentCommand
	var lastError *string
	var lockedUntil *time.Time
	var processedAt *time.Time
	var failedAt *time.Time

	err := tx.QueryRow(ctx, `
		SELECT id, command_type, property_id, source_id, payload, idempotency_key,
		       status, attempt_count, max_attempts, last_error, locked_until, next_retry_at,
		       created_at, processed_at, failed_at
		FROM payment_commands
		WHERE id = $1
		  AND status IN ('pending', 'processing')
		  AND (locked_until IS NULL OR locked_until <= now())
		FOR UPDATE SKIP LOCKED`, id,
	).Scan(
		&cmd.ID, &cmd.CommandType, &cmd.PropertyID, &cmd.SourceID, &cmd.Payload, &cmd.IdempotencyKey,
		&cmd.Status, &cmd.AttemptCount, &cmd.MaxAttempts, &lastError, &lockedUntil, &cmd.NextRetryAt,
		&cmd.CreatedAt, &processedAt, &failedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil // Locked by concurrent worker or already processed
		}
		return nil, err
	}

	cmd.AttemptCount++
	leaseEnd := time.Now().UTC().Add(leaseDuration)
	_, err = tx.Exec(ctx, `
		UPDATE payment_commands
		SET status = 'processing',
		    locked_until = $2,
		    attempt_count = $3
		WHERE id = $1`, cmd.ID, leaseEnd, cmd.AttemptCount)
	if err != nil {
		return nil, fmt.Errorf("claim payment command %d: %w", cmd.ID, err)
	}

	cmd.Status = domain.PaymentCommandStatusProcessing
	cmd.LockedUntil = &leaseEnd
	cmd.LastError = lastError
	cmd.ProcessedAt = processedAt
	cmd.FailedAt = failedAt
	return &cmd, nil
}

// MarkCompletedTx marks a payment command as completed inside the transaction.
func (r *PaymentCommandRepo) MarkCompletedTx(ctx context.Context, tx pgx.Tx, id int64) error {
	_, err := tx.Exec(ctx, `
		UPDATE payment_commands
		SET status = 'completed',
		    processed_at = now(),
		    locked_until = NULL
		WHERE id = $1`, id)
	return err
}

// RecordCommandFailureTx records failure details, schedules next retry, or dead-letters on max attempts or max retry age.
func (r *PaymentCommandRepo) RecordCommandFailureTx(
	ctx context.Context,
	tx pgx.Tx,
	cmd *domain.PaymentCommand,
	errorMsg string,
	retryDelay time.Duration,
) error {
	if cmd.AttemptCount >= cmd.MaxAttempts || (!cmd.CreatedAt.IsZero() && time.Since(cmd.CreatedAt) > domain.MaxPaymentCommandRetryAge) {
		reason := "exceeded max attempts"
		if cmd.AttemptCount < cmd.MaxAttempts {
			reason = "exceeded max retry age"
		}
		slog.Error("CRITICAL: payment command dead-lettered - "+reason,
			"command_id", cmd.ID,
			"command_type", cmd.CommandType,
			"source_id", cmd.SourceID,
			"idempotency_key", cmd.IdempotencyKey,
			"attempts", cmd.AttemptCount,
			"created_at", cmd.CreatedAt,
			"error", errorMsg,
		)
		cmd.Status = domain.PaymentCommandStatusDeadLetter
		_, err := tx.Exec(ctx, `
			UPDATE payment_commands
			SET status = 'dead_letter',
			    last_error = $2,
			    failed_at = now(),
			    locked_until = NULL
			WHERE id = $1`, cmd.ID, errorMsg)
		return err
	}

	cmd.Status = domain.PaymentCommandStatusPending
	nextRetry := time.Now().UTC().Add(retryDelay)
	_, err := tx.Exec(ctx, `
		UPDATE payment_commands
		SET status = 'pending',
		    last_error = $2,
		    next_retry_at = $3,
		    locked_until = NULL
		WHERE id = $1`, cmd.ID, errorMsg, nextRetry)
	return err
}
