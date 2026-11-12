package finance

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

// PaymentCommandAlerter alerts operators when a payment command permanently fails.
type PaymentCommandAlerter interface {
	NotifyPaymentCommandDeadLetter(ctx context.Context, cmd *domain.PaymentCommand, failureErr string) error
}

// PaymentCommandWorker processes pending asynchronous payment gateway commands.
type PaymentCommandWorker struct {
	pool             *pgxpool.Pool
	repo             *postgres.PaymentCommandRepo
	refundDispatcher RefundDispatcher
	alerter          PaymentCommandAlerter
}

func NewPaymentCommandWorker(
	pool *pgxpool.Pool,
	repo *postgres.PaymentCommandRepo,
	refundDispatcher RefundDispatcher,
	alerter ...PaymentCommandAlerter,
) *PaymentCommandWorker {
	var a PaymentCommandAlerter
	if len(alerter) > 0 {
		a = alerter[0]
	}
	return &PaymentCommandWorker{
		pool:             pool,
		repo:             repo,
		refundDispatcher: refundDispatcher,
		alerter:          a,
	}
}

// ProcessBatch processes up to limit pending payment commands.
func (w *PaymentCommandWorker) ProcessBatch(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 50
	}
	ids, err := w.repo.ListPendingCommandIDs(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("list pending payment commands: %w", err)
	}

	processedCount := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return processedCount, ctx.Err()
		}
		if err := w.ProcessSingleCommand(ctx, id); err != nil {
			slog.Error("failed processing payment command", "command_id", id, "err", err)
		} else {
			processedCount++
		}
	}
	return processedCount, nil
}

// ProcessSingleCommand locks, executes, and finalizes a single payment command under row lock.
func (w *PaymentCommandWorker) ProcessSingleCommand(ctx context.Context, id int64) error {
	leaseDuration := 2 * time.Minute

	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin payment command tx: %w", err)
	}
	defer tx.Rollback(ctx)

	cmd, err := w.repo.LockAndClaimCommandTx(ctx, tx, id, leaseDuration)
	if err != nil {
		return fmt.Errorf("lock and claim payment command %d: %w", id, err)
	}
	if cmd == nil {
		return nil // Already claimed or processed
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit claim payment command %d: %w", id, err)
	}

	// Execute command outside transaction to avoid holding locks during network I/O
	execErr := w.executeCommand(ctx, cmd)

	// Post-execution update in a new transaction
	updateTx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin post-execution tx: %w", err)
	}
	defer updateTx.Rollback(ctx)

	if execErr == nil {
		if err := w.repo.MarkCompletedTx(ctx, updateTx, cmd.ID); err != nil {
			return fmt.Errorf("mark payment command %d completed: %w", cmd.ID, err)
		}
		return updateTx.Commit(ctx)
	}

	retryDelay := w.computeBackoff(cmd.AttemptCount)
	isDeadLetter := cmd.AttemptCount >= cmd.MaxAttempts || time.Since(cmd.CreatedAt) > domain.MaxPaymentCommandRetryAge
	if err := w.repo.RecordCommandFailureTx(ctx, updateTx, cmd, execErr.Error(), retryDelay); err != nil {
		return fmt.Errorf("record payment command failure %d: %w", cmd.ID, err)
	}

	if err := updateTx.Commit(ctx); err != nil {
		return fmt.Errorf("commit post-execution failure %d: %w", cmd.ID, err)
	}

	if isDeadLetter && w.alerter != nil {
		_ = w.alerter.NotifyPaymentCommandDeadLetter(ctx, cmd, execErr.Error())
	}

	return execErr
}

func (w *PaymentCommandWorker) executeCommand(ctx context.Context, cmd *domain.PaymentCommand) error {
	switch cmd.CommandType {
	case domain.PaymentCommandRefundDispatch:
		if w.refundDispatcher == nil {
			return fmt.Errorf("refund dispatcher not configured")
		}
		var p domain.RefundDispatchPayload
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return fmt.Errorf("unmarshal refund dispatch payload: %w", err)
		}
		pctx := ctx
		if cmd.PropertyID != p.PropertyID && p.PropertyID != [16]byte{} {
			pctx = requestscope.WithPropertyID(ctx, p.PropertyID)
		} else if cmd.PropertyID != [16]byte{} {
			pctx = requestscope.WithPropertyID(ctx, cmd.PropertyID)
		}
		return w.refundDispatcher.DispatchRefund(pctx, p.RefundID)
	default:
		return fmt.Errorf("unrecognized payment command type '%s'", cmd.CommandType)
	}
}

func (w *PaymentCommandWorker) computeBackoff(attempt int) time.Duration {
	base := 5.0
	multiplier := 2.0
	seconds := base * math.Pow(multiplier, float64(attempt-1))

	// Add 0% to 25% pseudo-random jitter
	jitterFrac := 0.0
	var n [1]byte
	if _, err := rand.Read(n[:]); err == nil {
		jitterFrac = float64(n[0]) / 255.0 * 0.25
	}
	seconds = seconds * (1.0 + jitterFrac)

	// Cap delay at 900 seconds (15 minutes) AFTER applying jitter
	if seconds > 900.0 {
		seconds = 900.0
	}
	return time.Duration(seconds) * time.Second
}

// Start runs the payment command worker in a background loop until ctx is cancelled.
func (w *PaymentCommandWorker) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := w.ProcessBatch(ctx, 25); err != nil {
				slog.Error("payment command worker error", "err", err)
			}
		}
	}
}
