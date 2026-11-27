package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type outboxRow struct {
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

type fakeOutboxDB struct {
	rows        map[int64]*outboxRow
	lastClaimSQL string
}

func newFakeOutboxDB() *fakeOutboxDB {
	return &fakeOutboxDB{rows: make(map[int64]*outboxRow)}
}

func normalizeSQL(sql string) string {
	return strings.ToLower(strings.Join(strings.Fields(sql), " "))
}

func (f *fakeOutboxDB) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	norm := normalizeSQL(sql)
	if strings.Contains(norm, "set lease_token = null") && strings.Contains(norm, "next_retry_at = $2") {
		id := args[0].(int64)
		nextRetryAt := args[1].(time.Time)
		r, ok := f.rows[id]
		if !ok {
			return pgconn.NewCommandTag("UPDATE 0"), nil
		}
		if len(args) == 3 {
			if !strings.Contains(norm, "lease_token = $3") {
				return pgconn.CommandTag{}, fmt.Errorf("SQL contract violation in RescheduleLease: missing lease_token = $3 guard")
			}
			tok := args[2].(uuid.UUID)
			if r.leaseToken == nil || *r.leaseToken != tok {
				return pgconn.NewCommandTag("UPDATE 0"), nil
			}
		}
		r.leaseToken = nil
		r.leaseExpiresAt = nil
		r.nextRetryAt = &nextRetryAt
		return pgconn.NewCommandTag("UPDATE 1"), nil
	}
	return pgconn.NewCommandTag("OK 0"), nil
}

func (f *fakeOutboxDB) Query(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
	return nil, nil
}

func (f *fakeOutboxDB) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	norm := normalizeSQL(sql)
	if strings.Contains(norm, "update ledger_outbox_events") && strings.Contains(norm, "set lease_token = $2") {
		f.lastClaimSQL = norm
		requiredPredicates := []string{
			"processed_at is null",
			"failed_at is null",
			"(lease_expires_at is null or lease_expires_at <= $4)",
			"(next_retry_at is null or next_retry_at <= $4)",
		}
		for _, pred := range requiredPredicates {
			if !strings.Contains(norm, pred) {
				return &fakeOutboxRow{err: fmt.Errorf("SQL contract violation in ClaimEvent: missing %q", pred)}
			}
		}
		id := args[0].(int64)
		leaseToken := args[1].(uuid.UUID)
		leaseExpiry := args[2].(time.Time)
		now := args[3].(time.Time)
		r, ok := f.rows[id]
		if !ok || r.processedAt != nil || r.failedAt != nil {
			return &fakeOutboxRow{err: pgx.ErrNoRows}
		}
		if r.leaseExpiresAt != nil && r.leaseExpiresAt.After(now) {
			return &fakeOutboxRow{err: pgx.ErrNoRows}
		}
		if r.nextRetryAt != nil && r.nextRetryAt.After(now) {
			return &fakeOutboxRow{err: pgx.ErrNoRows}
		}
		r.leaseToken = &leaseToken
		r.leaseExpiresAt = &leaseExpiry
		return &fakeOutboxRow{row: r}
	}
	return &fakeOutboxRow{err: pgx.ErrNoRows}
}

type fakeOutboxRow struct {
	row *outboxRow
	err error
}

func (r *fakeOutboxRow) Scan(dest ...any) error {
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

func TestLedgerOutbox_ClaimEventEnforcesNextRetryAtAndRescheduleGuard(t *testing.T) {
	db := newFakeOutboxDB()
	repo := NewLedgerOutboxRepo(db)
	ctx := context.Background()

	db.rows[42] = &outboxRow{
		id:             42,
		eventType:      "payout_batch_transfer",
		propertyID:     uuid.New(),
		sourceID:       uuid.New(),
		payload:        json.RawMessage(`{}`),
		idempotencyKey: "k1",
		attemptCount:   0,
		maxAttempts:    5,
		createdAt:      time.Now().UTC(),
	}

	// 1. Worker A claims event 42.
	evt, tokA, err := repo.ClaimEvent(ctx, 42, 60*time.Second)
	if err != nil || evt == nil || tokA == uuid.Nil {
		t.Fatalf("expected initial claim to succeed, got evt=%v tok=%v err=%v", evt, tokA, err)
	}

	// 2. Lease-token mismatch on RescheduleLease must fail and leave lease intact.
	wrongTok := uuid.New()
	if err := repo.RescheduleLease(ctx, 42, wrongTok, time.Now().UTC().Add(10*time.Second)); err == nil {
		t.Fatal("expected RescheduleLease with mismatched token to fail")
	}

	// 3. Worker A encounters busy batch lease -> RescheduleLease with future next_retry_at.
	futureRetry := time.Now().UTC().Add(30 * time.Second)
	if err := repo.RescheduleLease(ctx, 42, tokA, futureRetry); err != nil {
		t.Fatalf("expected RescheduleLease to succeed, got %v", err)
	}
	if db.rows[42].attemptCount != 0 || db.rows[42].processedAt != nil {
		t.Fatalf("expected attemptCount=0 and processedAt=nil after RescheduleLease, got attempts=%d processed=%v",
			db.rows[42].attemptCount, db.rows[42].processedAt)
	}

	// 4. Stale candidate: Worker B holds ID 42 from an earlier FetchPendingCandidateIDs and tries ClaimEvent before next_retry_at.
	staleEvt, staleTok, err := repo.ClaimEvent(ctx, 42, 60*time.Second)
	if err != nil {
		t.Fatalf("unexpected error on early ClaimEvent: %v", err)
	}
	if staleEvt != nil || staleTok != uuid.Nil {
		t.Fatalf("expected ClaimEvent before next_retry_at to be rejected, got evt=%v", staleEvt)
	}

	// 5. Once next_retry_at is in the past, ClaimEvent succeeds.
	pastRetry := time.Now().UTC().Add(-1 * time.Second)
	db.rows[42].nextRetryAt = &pastRetry
	readyEvt, readyTok, err := repo.ClaimEvent(ctx, 42, 60*time.Second)
	if err != nil || readyEvt == nil || readyTok == uuid.Nil {
		t.Fatalf("expected ClaimEvent after next_retry_at to succeed, got evt=%v tok=%v err=%v", readyEvt, readyTok, err)
	}
}
