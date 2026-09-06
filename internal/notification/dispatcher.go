package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// DispatchRepo is the set of outbox and notification methods the Dispatcher uses.
type DispatchRepo interface {
	FetchCandidateIDs(ctx context.Context, limit int) ([]int64, error)
	LockAndFetchEventTx(ctx context.Context, tx pgx.Tx, id int64) (*domain.OutboxEvent, error)
	MarkDispatchedTx(ctx context.Context, tx pgx.Tx, id int64) error
	RecordEventFailureTx(ctx context.Context, tx pgx.Tx, id int64, newAttempts int, errorJSON string, nextRetryAt time.Time, isDeadLetter bool) error
	CreateOrGetNotificationTx(ctx context.Context, tx pgx.Tx, n *domain.Notification) (uuid.UUID, error)
	RecordDeliveryTx(ctx context.Context, tx pgx.Tx, d *domain.NotificationDelivery) error
}

// RecipientResolver resolves which user IDs should receive a notification
// for a given outbox event. Returns a list of Notification templates with
// RecipientID, Type, Title, DeepLink, and IsActionRequired populated.
// An empty list with no error means "valid business state with zero recipients"
// (e.g. no manager assigned yet) — not a failure.
type RecipientResolver interface {
	Resolve(ctx context.Context, evt *domain.OutboxEvent) ([]domain.Notification, error)
}

// DispatcherConfig holds tunable dispatcher parameters.
type DispatcherConfig struct {
	// BatchSize is the number of candidate IDs fetched per poll tick.
	BatchSize int
	// PollInterval is how often the dispatcher wakes up when there are events.
	PollInterval time.Duration
	// IdleInterval is how often the dispatcher wakes up when the queue is empty.
	IdleInterval time.Duration
	// MaxRetries is how many total attempts before an event is dead-lettered.
	MaxRetries int
}

func DefaultConfig() DispatcherConfig {
	return DispatcherConfig{
		BatchSize:    50,
		PollInterval: 500 * time.Millisecond,
		IdleInterval: 2 * time.Second,
		MaxRetries:   5,
	}
}

// Dispatcher is a background worker that polls outbox_events and fans them out
// to recipient-specific Notification rows.
//
// Concurrency and idempotency guarantees:
//   - FetchCandidateIDs is lock-free; each event is locked individually via
//     LockAndFetchEventTx (FOR UPDATE SKIP LOCKED).
//   - The row lock is held for the event's entire processing window (including
//     failure recording) so concurrent dispatcher instances during rolling
//     deploys cannot double-process or double-increment attempt_count.
//   - Each recipient's notification write uses a nested savepoint so one bad
//     recipient never rolls back a sibling recipient's already-committed write.
//   - Crash dedup: UNIQUE (source_event_id, recipient_id) + ON CONFLICT DO NOTHING.
type Dispatcher struct {
	pool     *pgxpool.Pool
	repo     DispatchRepo
	resolver RecipientResolver
	cfg      DispatcherConfig
	log      *slog.Logger
}

func NewDispatcher(pool *pgxpool.Pool, repo DispatchRepo, resolver RecipientResolver, cfg DispatcherConfig, log *slog.Logger) *Dispatcher {
	if cfg.BatchSize == 0 {
		cfg = DefaultConfig()
	}
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{pool: pool, repo: repo, resolver: resolver, cfg: cfg, log: log}
}

// Run starts the dispatcher loop. It blocks until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	d.log.Info("notification dispatcher started", "batch_size", d.cfg.BatchSize, "max_retries", d.cfg.MaxRetries)
	for {
		processed := d.tick(ctx)
		interval := d.cfg.IdleInterval
		if processed > 0 {
			interval = d.cfg.PollInterval
		}
		select {
		case <-ctx.Done():
			d.log.Info("notification dispatcher stopped")
			return
		case <-time.After(interval):
		}
	}
}

// tick fetches one batch of candidate IDs and processes each one.
// Returns the number of events actually processed (locked and attempted).
func (d *Dispatcher) tick(ctx context.Context) int {
	ids, err := d.repo.FetchCandidateIDs(ctx, d.cfg.BatchSize)
	if err != nil {
		d.log.Error("outbox: FetchCandidateIDs failed", "err", err)
		return 0
	}
	processed := 0
	for _, id := range ids {
		if ctx.Err() != nil {
			return processed
		}
		d.processEvent(ctx, id)
		processed++
	}
	return processed
}

// processEvent processes a single outbox event within its own transaction.
// The transaction holds the FOR UPDATE SKIP LOCKED row lock for the event's
// full processing window — including any failure recording — so the lock is
// only released once the outcome is committed.
func (d *Dispatcher) processEvent(ctx context.Context, id int64) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		d.log.Error("outbox: begin tx failed", "event_id", id, "err", err)
		return
	}
	// Always rollback if we return without committing; pgx ignores rollback of
	// an already-committed tx, so this is safe in both branches.
	defer func() { _ = tx.Rollback(ctx) }()

	evt, err := d.repo.LockAndFetchEventTx(ctx, tx, id)
	if err != nil {
		d.log.Error("outbox: lock event failed", "event_id", id, "err", err)
		return
	}
	if evt == nil {
		// Another worker holds the lock or the event was already dispatched/failed.
		return
	}

	succeeded, failed := d.resolveAndDeliver(ctx, tx, evt)
	d.finalise(ctx, tx, evt, succeeded, failed)
}

// resolveAndDeliver calls the RecipientResolver (with panic recovery) then
// writes one Notification + one DeliveryTx per recipient using nested savepoints.
// Failures in resolution or individual recipient writes populate the returned slices
// but do not abort sibling recipients.
func (d *Dispatcher) resolveAndDeliver(
	ctx context.Context,
	tx pgx.Tx,
	evt *domain.OutboxEvent,
) (succeeded []uuid.UUID, failed []domain.RecipientFailure) {

	// Protected recipient resolution — panics and errors both route to the
	// same failure outcome so no seam is left uncovered.
	recipients, resolveErr := d.safeResolve(ctx, evt)
	if resolveErr != nil {
		failed = append(failed, domain.RecipientFailure{
			RecipientID: uuid.Nil,
			Error:       fmt.Sprintf("recipient resolution: %s", resolveErr),
		})
		return // skip per-recipient loop; fall through to finalise
	}

	for _, tmpl := range recipients {
		tmpl.SourceEventID = &evt.ID
		notifID, spErr := d.deliverRecipient(ctx, tx, &tmpl)
		if spErr != nil {
			failed = append(failed, domain.RecipientFailure{
				RecipientID: tmpl.RecipientID,
				Error:       spErr.Error(),
			})
		} else {
			_ = notifID // notifID already used by RecordDeliveryTx inside deliverRecipient
			succeeded = append(succeeded, tmpl.RecipientID)
		}
	}
	return
}

// safeResolve wraps resolver.Resolve with a recover() so panics from malformed
// payloads or nil-pointer dereferences are captured as errors instead of
// crashing the dispatcher goroutine or leaking the pool connection.
func (d *Dispatcher) safeResolve(ctx context.Context, evt *domain.OutboxEvent) (recipients []domain.Notification, retErr error) {
	defer func() {
		if r := recover(); r != nil {
			retErr = fmt.Errorf("panic in resolver: %v", r)
		}
	}()
	return d.resolver.Resolve(ctx, evt)
}

// deliverRecipient runs one recipient's notification insert and delivery record
// inside a nested savepoint. If the savepoint work fails, only that recipient's
// changes are rolled back — sibling recipients committed earlier in the outer
// transaction are unaffected.
func (d *Dispatcher) deliverRecipient(ctx context.Context, tx pgx.Tx, n *domain.Notification) (uuid.UUID, error) {
	sp, err := tx.Begin(ctx) // nested savepoint
	if err != nil {
		return uuid.Nil, fmt.Errorf("begin savepoint: %w", err)
	}

	notifID, err := d.repo.CreateOrGetNotificationTx(ctx, sp, n)
	if err != nil {
		_ = sp.Rollback(ctx)
		return uuid.Nil, fmt.Errorf("create notification: %w", err)
	}

	delivery := &domain.NotificationDelivery{
		ID:             uuid.New(),
		NotificationID: notifID,
		Channel:        "inapp",
		Status:         "sent",
		AttemptCount:   1,
	}
	if err := d.repo.RecordDeliveryTx(ctx, sp, delivery); err != nil {
		_ = sp.Rollback(ctx)
		return uuid.Nil, fmt.Errorf("record delivery: %w", err)
	}

	if err := sp.Commit(ctx); err != nil {
		return uuid.Nil, fmt.Errorf("commit savepoint: %w", err)
	}
	return notifID, nil
}

// finalise either marks the event dispatched (all recipients ok) or records the
// failure with deterministic exponential backoff, within the same tx that holds
// the FOR UPDATE lock — so no concurrent worker can race the attempt_count.
func (d *Dispatcher) finalise(
	ctx context.Context,
	tx pgx.Tx,
	evt *domain.OutboxEvent,
	succeeded []uuid.UUID,
	failed []domain.RecipientFailure,
) {
	if len(failed) == 0 {
		if err := d.repo.MarkDispatchedTx(ctx, tx, evt.ID); err != nil {
			d.log.Error("outbox: MarkDispatchedTx failed", "event_id", evt.ID, "err", err)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			d.log.Error("outbox: commit dispatch failed", "event_id", evt.ID, "err", err)
		}
		return
	}

	// Compute backoff in Go — not in SQL — to avoid Postgres multi-column SET
	// pre-update read ordering producing an off-by-one backoff step.
	newAttempts := evt.AttemptCount + 1
	isDeadLetter := newAttempts >= d.cfg.MaxRetries
	nextRetryAt := time.Now().UTC().Add(BackoffDuration(newAttempts))

	summary := domain.DispatchErrorSummary{Succeeded: succeeded, Failed: failed}
	if summary.Succeeded == nil {
		summary.Succeeded = []uuid.UUID{}
	}
	errorJSON, _ := json.Marshal(summary)

	if err := d.repo.RecordEventFailureTx(ctx, tx, evt.ID, newAttempts, string(errorJSON), nextRetryAt, isDeadLetter); err != nil {
		d.log.Error("outbox: RecordEventFailureTx failed", "event_id", evt.ID, "err", err)
		return
	}

	if isDeadLetter {
		d.log.Error("outbox event dead-lettered",
			"event_id", evt.ID,
			"event_type", evt.EventType,
			"property_id", evt.PropertyID,
			"succeeded_recipients", succeeded,
			"failed_recipients", failed,
		)
	} else {
		d.log.Warn("outbox event dispatch partial failure",
			"event_id", evt.ID,
			"event_type", evt.EventType,
			"attempt", newAttempts,
			"next_retry_at", nextRetryAt,
			"failed_recipients", failed,
		)
	}

	if err := tx.Commit(ctx); err != nil {
		d.log.Error("outbox: commit failure record failed", "event_id", evt.ID, "err", err)
	}
}
