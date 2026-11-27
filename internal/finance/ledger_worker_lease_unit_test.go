package finance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type workerLeaseFakeRow struct {
	id             int64
	eventType      string
	propertyID     uuid.UUID
	sourceID       uuid.UUID
	payload        []byte
	idempotencyKey string
	attemptCount   int
	maxAttempts    int
	lastError      *string
	nextRetryAt    *time.Time
	createdAt      time.Time
	processedAt    *time.Time
	failedAt       *time.Time
	leaseToken     *uuid.UUID
	leaseExpiresAt *time.Time
}

type workerLeaseFakeDB struct {
	row                  *workerLeaseFakeRow
	finalizeSuccessCalls int
	finalizeFailureCalls int
	rescheduleCalls      int
	onClaim              func(r *workerLeaseFakeRow)
}

func (db *workerLeaseFakeDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	norm := strings.ToLower(strings.Join(strings.Fields(sql), " "))
	switch {
	case strings.Contains(norm, "set processed_at = now()"):
		db.finalizeSuccessCalls++
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case strings.Contains(norm, "set attempt_count = $2"):
		db.finalizeFailureCalls++
		return pgconn.NewCommandTag("UPDATE 1"), nil
	case strings.Contains(norm, "set lease_token = null") && strings.Contains(norm, "next_retry_at = $2"):
		db.rescheduleCalls++
		id := args[0].(int64)
		nextRetryAt := args[1].(time.Time)
		if db.row == nil || db.row.id != id {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		if len(args) == 3 {
			tok := args[2].(uuid.UUID)
			if db.row.leaseToken == nil || *db.row.leaseToken != tok {
				return pgconn.NewCommandTag("UPDATE 0"), nil
			}
		}
		db.row.leaseToken = nil
		db.row.leaseExpiresAt = nil
		db.row.nextRetryAt = &nextRetryAt
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	return pgconn.NewCommandTag("OK 0"), nil
}

func (db *workerLeaseFakeDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, nil
}

func (db *workerLeaseFakeDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	norm := strings.ToLower(strings.Join(strings.Fields(sql), " "))
	if strings.Contains(norm, "update ledger_outbox_events") && strings.Contains(norm, "set lease_token = $2") {
		id := args[0].(int64)
		leaseToken := args[1].(uuid.UUID)
		leaseExpiry := args[2].(time.Time)
		now := args[3].(time.Time)
		r := db.row
		if r == nil || r.id != id || r.processedAt != nil || r.failedAt != nil {
			return &workerLeasePGXRow{err: pgx.ErrNoRows}
		}
		if r.leaseExpiresAt != nil && r.leaseExpiresAt.After(now) {
			return &workerLeasePGXRow{err: pgx.ErrNoRows}
		}
		if r.nextRetryAt != nil && r.nextRetryAt.After(now) {
			return &workerLeasePGXRow{err: pgx.ErrNoRows}
		}
		r.leaseToken = &leaseToken
		r.leaseExpiresAt = &leaseExpiry
		if db.onClaim != nil {
			db.onClaim(r)
		}
		return &workerLeasePGXRow{row: r}
	}
	return &workerLeasePGXRow{err: pgx.ErrNoRows}
}

type workerLeasePGXRow struct {
	row *workerLeaseFakeRow
	err error
}

func (r *workerLeasePGXRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*int64) = r.row.id
	*dest[1].(*string) = r.row.eventType
	*dest[2].(*uuid.UUID) = r.row.propertyID
	*dest[3].(*uuid.UUID) = r.row.sourceID
	*dest[4].(*json.RawMessage) = r.row.payload
	*dest[5].(*string) = r.row.idempotencyKey
	*dest[6].(*int) = r.row.attemptCount
	*dest[7].(*int) = r.row.maxAttempts
	*dest[8].(**string) = r.row.lastError
	*dest[9].(**time.Time) = r.row.nextRetryAt
	*dest[10].(*time.Time) = r.row.createdAt
	*dest[11].(**time.Time) = r.row.processedAt
	*dest[12].(**time.Time) = r.row.failedAt
	return nil
}

type stubBusyPayoutDispatcher struct {
	onDispatch func()
	err        error
}

func (s *stubBusyPayoutDispatcher) DispatchBatch(_ context.Context, _ uuid.UUID) error {
	if s.onDispatch != nil {
		s.onDispatch()
	}
	return s.err
}

func TestLedgerOutboxWorker_ProcessSingleEvent_ReschedulesOnBatchLeaseBusy(t *testing.T) {
	batchID := uuid.New()
	db := &workerLeaseFakeDB{
		row: &workerLeaseFakeRow{
			id:             101,
			eventType:      "payout_batch_transfer",
			propertyID:     uuid.New(),
			sourceID:       batchID,
			payload:        json.RawMessage(`{"batch_id":"` + batchID.String() + `"}`),
			idempotencyKey: "payout_batch:" + batchID.String(),
			attemptCount:   0,
			maxAttempts:    5,
			createdAt:      time.Now().UTC(),
		},
	}

	repo := postgres.NewLedgerOutboxRepo(db)
	worker := NewLedgerOutboxWorker(nil, repo, nil)
	worker.SetPayoutDispatcher(&stubBusyPayoutDispatcher{err: ErrPayoutBatchLeaseBusy})

	before := time.Now().UTC()
	if err := worker.ProcessSingleEvent(context.Background(), 101); err != nil {
		t.Fatalf("expected nil error on ErrPayoutBatchLeaseBusy, got %v", err)
	}

	if db.finalizeSuccessCalls != 0 {
		t.Fatalf("expected FinalizeSuccess never to be called, got %d", db.finalizeSuccessCalls)
	}
	if db.finalizeFailureCalls != 0 {
		t.Fatalf("expected FinalizeFailure never to be called, got %d", db.finalizeFailureCalls)
	}
	if db.rescheduleCalls != 1 {
		t.Fatalf("expected RescheduleLease to be called once, got %d", db.rescheduleCalls)
	}
	if db.row.attemptCount != 0 {
		t.Fatalf("expected attempt_count to remain 0, got %d", db.row.attemptCount)
	}
	if db.row.leaseToken != nil || db.row.leaseExpiresAt != nil {
		t.Fatalf("expected lease to be cleared after reschedule, got token=%v exp=%v", db.row.leaseToken, db.row.leaseExpiresAt)
	}
	if db.row.nextRetryAt == nil || !db.row.nextRetryAt.After(before) {
		t.Fatalf("expected next_retry_at in the future, got %v", db.row.nextRetryAt)
	}
}

func TestLedgerOutboxWorker_ProcessSingleEvent_MismatchedLeaseCannotReschedule(t *testing.T) {
	batchID := uuid.New()
	otherWorkerToken := uuid.New()
	db := &workerLeaseFakeDB{
		row: &workerLeaseFakeRow{
			id:             202,
			eventType:      "payout_batch_transfer",
			propertyID:     uuid.New(),
			sourceID:       batchID,
			payload:        json.RawMessage(`{"batch_id":"` + batchID.String() + `"}`),
			idempotencyKey: "payout_batch:" + batchID.String(),
			attemptCount:   0,
			maxAttempts:    5,
			createdAt:      time.Now().UTC(),
		},
	}

	repo := postgres.NewLedgerOutboxRepo(db)
	worker := NewLedgerOutboxWorker(nil, repo, nil)
	worker.SetPayoutDispatcher(&stubBusyPayoutDispatcher{
		err: ErrPayoutBatchLeaseBusy,
		onDispatch: func() {
			// Simulate another worker taking over the outbox lease while dispatch was in flight.
			db.row.leaseToken = &otherWorkerToken
		},
	})

	err := worker.ProcessSingleEvent(context.Background(), 202)
	if err == nil {
		t.Fatal("expected error when RescheduleLease encounters a superseded lease token")
	}
	if db.finalizeSuccessCalls != 0 {
		t.Fatalf("expected FinalizeSuccess never to be called, got %d", db.finalizeSuccessCalls)
	}
	if db.row.attemptCount != 0 {
		t.Fatalf("expected attempt_count to remain 0, got %d", db.row.attemptCount)
	}
	if db.row.leaseToken == nil || *db.row.leaseToken != otherWorkerToken {
		t.Fatalf("expected other worker's lease token %s to remain intact, got %v", otherWorkerToken, db.row.leaseToken)
	}
	if db.row.nextRetryAt != nil {
		t.Fatalf("expected next_retry_at to remain nil when reschedule is rejected, got %v", db.row.nextRetryAt)
	}
}
