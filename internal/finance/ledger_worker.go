package finance

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// LedgerMirrorer defines the interface for double-entry mirror posting.
type LedgerMirrorer interface {
	MirrorDepartureSettlement(
		ctx context.Context,
		propertyID, departureID uuid.UUID,
		depositPaise, unusedRentReversal, damagesPaise, netRefundPaise, outstandingDuesNettedPaise, receivableBalancePaise int64,
		at time.Time,
	) error
	MirrorPaymentAllocations(
		ctx context.Context,
		propertyID uuid.UUID,
		p *domain.Payment,
		allocations []PaymentAllocationItem,
		unappliedPaise int64,
	) error
	MirrorUnappliedPayment(
		ctx context.Context,
		propertyID, paymentID uuid.UUID,
		amountPaise int64,
		at time.Time,
	) error
}

// LedgerOutboxWorker polls and processes pending ledger outbox events to ensure zero ledger desynchronization.
type LedgerOutboxWorker struct {
	pool             *pgxpool.Pool
	repo             *postgres.LedgerOutboxRepo
	mirrorer         LedgerMirrorer
	alerter          DeadLetterNotifier
	payoutDispatcher PayoutBatchDispatcher
}

func NewLedgerOutboxWorker(pool *pgxpool.Pool, repo *postgres.LedgerOutboxRepo, mirrorer LedgerMirrorer, alerter ...DeadLetterNotifier) *LedgerOutboxWorker {
	var a DeadLetterNotifier
	if len(alerter) > 0 {
		a = alerter[0]
	}
	return &LedgerOutboxWorker{
		pool:     pool,
		repo:     repo,
		mirrorer: mirrorer,
		alerter:  a,
	}
}

// SetPayoutDispatcher sets the dispatcher for payout_batch_transfer events.
func (w *LedgerOutboxWorker) SetPayoutDispatcher(d PayoutBatchDispatcher) {
	w.payoutDispatcher = d
}

// SetAlerter sets or replaces the dead-letter notifier for this worker.
func (w *LedgerOutboxWorker) SetAlerter(a DeadLetterNotifier) {
	w.alerter = a
}

// ProcessBatch processes a batch of eligible pending ledger outbox events.
func (w *LedgerOutboxWorker) ProcessBatch(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 50
	}
	ids, err := w.repo.FetchPendingCandidateIDs(ctx, limit)
	if err != nil {
		return 0, fmt.Errorf("fetch pending candidate ids: %w", err)
	}

	processedCount := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return processedCount, ctx.Err()
		}
		if err := w.ProcessSingleEvent(ctx, id); err != nil {
			slog.Error("failed processing ledger outbox event", "event_id", id, "err", err)
		} else {
			processedCount++
		}
	}
	return processedCount, nil
}

// ProcessSingleEvent processes a single event under a row lock (FOR UPDATE SKIP LOCKED).
func (w *LedgerOutboxWorker) ProcessSingleEvent(ctx context.Context, id int64) error {
	tx, err := w.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin worker tx: %w", err)
	}
	defer tx.Rollback(ctx)

	evt, err := w.repo.LockAndFetchEventTx(ctx, tx, id)
	if err != nil {
		return fmt.Errorf("lock and fetch event: %w", err)
	}
	if evt == nil {
		return nil // Already processed or locked by another worker
	}

	dispatchErr := w.dispatch(ctx, evt)
	if dispatchErr == nil {
		if err := w.repo.MarkProcessedTx(ctx, tx, evt.ID); err != nil {
			return fmt.Errorf("mark processed tx: %w", err)
		}
		return tx.Commit(ctx)
	}

	// Calculate exponential backoff
	newAttempts := evt.AttemptCount + 1
	isDeadLetter := newAttempts >= evt.MaxAttempts
	var nextRetry *time.Time
	if !isDeadLetter {
		backoffSec := math.Pow(2, float64(newAttempts))
		if backoffSec > 3600 {
			backoffSec = 3600
		}
		t := time.Now().UTC().Add(time.Duration(backoffSec) * time.Second)
		nextRetry = &t
	}

	if err := w.repo.RecordEventFailureTx(ctx, tx, evt, newAttempts, dispatchErr.Error(), nextRetry, isDeadLetter); err != nil {
		return fmt.Errorf("record event failure tx: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit failure tx: %w", err)
	}

	if isDeadLetter {
		slog.Error("CRITICAL: ledger outbox event exceeded max attempts - financial books desynchronized",
			"event_id", evt.ID,
			"event_type", evt.EventType,
			"source_id", evt.SourceID,
			"property_id", evt.PropertyID,
			"attempts", newAttempts,
			"error", dispatchErr.Error(),
		)
		if w.alerter != nil {
			if alertErr := w.alerter.NotifyDeadLetter(ctx, evt, dispatchErr.Error()); alertErr != nil {
				slog.Error("failed to dispatch dead-letter alert", "event_id", evt.ID, "err", alertErr)
			}
		}
	}

	return dispatchErr
}

func (w *LedgerOutboxWorker) dispatch(ctx context.Context, evt *domain.LedgerOutboxEvent) error {
	switch evt.EventType {
	case "departure_settlement_mirror":
		var p domain.DepartureSettlementMirrorPayload
		if err := json.Unmarshal(evt.Payload, &p); err != nil {
			return fmt.Errorf("unmarshal departure settlement mirror payload: %w", err)
		}
		if w.mirrorer == nil {
			return fmt.Errorf("mirrorer not configured")
		}
		return w.mirrorer.MirrorDepartureSettlement(
			ctx,
			p.PropertyID,
			p.DepartureID,
			p.DepositAmountPaise,
			p.UnusedRentRefundPaise,
			p.TotalDeductions,
			p.NetRefundPaise,
			p.OutstandingDuesNettedPaise,
			p.ReceivableBalancePaise,
			p.OccurredAt,
		)
	case "payout_batch_transfer":
		if w.payoutDispatcher == nil {
			return fmt.Errorf("payout batch dispatcher not configured")
		}
		batchID, err := UnmarshalPayoutBatchPayload(evt.Payload)
		if err != nil {
			return fmt.Errorf("unmarshal payout batch transfer payload: %w", err)
		}
		return w.payoutDispatcher.DispatchBatch(ctx, batchID)
	case "payment_mirror":
		var p domain.PaymentMirrorPayload
		if err := json.Unmarshal(evt.Payload, &p); err != nil {
			return fmt.Errorf("unmarshal payment mirror payload: %w", err)
		}
		if w.mirrorer == nil {
			return fmt.Errorf("mirrorer not configured")
		}
		if p.IsUnapplied {
			return w.mirrorer.MirrorUnappliedPayment(ctx, p.PropertyID, p.PaymentID, p.AmountPaise, p.MatchedAt)
		}
		allocs := make([]PaymentAllocationItem, 0, len(p.Allocations))
		for _, a := range p.Allocations {
			allocs = append(allocs, PaymentAllocationItem{
				AmountPaise: a.AmountPaise,
				DueKind:     domain.DueKind(a.DueKind),
			})
		}
		paymentObj := &domain.Payment{
			ID:        p.PaymentID,
			Amount:    p.AmountPaise,
			MatchedBy: p.MatchedBy,
			MatchedAt: p.MatchedAt,
		}
		return w.mirrorer.MirrorPaymentAllocations(ctx, p.PropertyID, paymentObj, allocs, p.UnappliedPaise)
	default:
		return fmt.Errorf("unrecognized ledger outbox event type '%s'", evt.EventType)
	}
}
