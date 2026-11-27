package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestLedgerOutbox_ClaimEventAndRescheduleLease_Live(t *testing.T) {
	pool, _ := setupLiveTestPool(t, 10*time.Second)
	ctx := context.Background()
	repo := NewLedgerOutboxRepo(pool)

	propID := uuid.New()
	inviteCode := fmt.Sprintf("L%s", uuid.New().String()[:7])
	if _, err := pool.Exec(ctx, `
		INSERT INTO properties (id, name, address, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
		VALUES ($1, 'Outbox Live Test PG', '123 Test St', '+919999977777', 'outbox@upi', 'Outbox Owner', 'outbox@test.com', $2)`,
		propID, inviteCode,
	); err != nil {
		t.Fatalf("insert test property: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM properties WHERE id = $1`, propID)
	})

	sourceID := uuid.New()
	idemKey := fmt.Sprintf("live-outbox-lease-test-%s", uuid.New())

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	evt := &domain.LedgerOutboxEvent{
		EventType:      "payout_batch_transfer",
		PropertyID:     propID,
		SourceID:       sourceID,
		Payload:        json.RawMessage(`{"batch_id":"` + sourceID.String() + `"}`),
		IdempotencyKey: idemKey,
		MaxAttempts:    5,
	}
	if err := repo.InsertLedgerOutboxEventTx(ctx, tx, evt); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("insert outbox event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit tx: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM ledger_outbox_events WHERE id = $1`, evt.ID)
	})

	// 1. Worker A claims event.
	claimed, tokA, err := repo.ClaimEvent(ctx, evt.ID, 60*time.Second)
	if err != nil || claimed == nil || tokA == uuid.Nil {
		t.Fatalf("expected ClaimEvent to succeed, got evt=%v tok=%v err=%v", claimed, tokA, err)
	}

	// 2. Mismatched lease token must fail.
	if err := repo.RescheduleLease(ctx, evt.ID, uuid.New(), time.Now().UTC().Add(30*time.Second)); err == nil {
		t.Fatal("expected RescheduleLease with mismatched token to fail")
	}

	// 3. Valid lease token reschedules into the future without incrementing attempt_count.
	if err := repo.RescheduleLease(ctx, evt.ID, tokA, time.Now().UTC().Add(30*time.Second)); err != nil {
		t.Fatalf("RescheduleLease failed: %v", err)
	}

	// 4. PostgreSQL itself must reject ClaimEvent while next_retry_at > now.
	earlyEvt, earlyTok, err := repo.ClaimEvent(ctx, evt.ID, 60*time.Second)
	if err != nil {
		t.Fatalf("unexpected error on early ClaimEvent: %v", err)
	}
	if earlyEvt != nil || earlyTok != uuid.Nil {
		t.Fatalf("expected PostgreSQL ClaimEvent before next_retry_at to return nil, got evt=%v", earlyEvt)
	}

	// 5. Once next_retry_at is in the past, ClaimEvent succeeds.
	if _, err := pool.Exec(ctx, `UPDATE ledger_outbox_events SET next_retry_at = now() - interval '1 second' WHERE id = $1`, evt.ID); err != nil {
		t.Fatalf("update next_retry_at: %v", err)
	}
	readyEvt, readyTok, err := repo.ClaimEvent(ctx, evt.ID, 60*time.Second)
	if err != nil || readyEvt == nil || readyTok == uuid.Nil {
		t.Fatalf("expected ClaimEvent after next_retry_at to succeed, got evt=%v tok=%v err=%v", readyEvt, readyTok, err)
	}
	if readyEvt.AttemptCount != 0 {
		t.Fatalf("expected attempt_count=0 after reschedule, got %d", readyEvt.AttemptCount)
	}
}
