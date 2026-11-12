package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockCommandTx struct {
	pgx.Tx
	lastSQL  string
	lastArgs []any
}

func (m *mockCommandTx) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	m.lastSQL = sql
	m.lastArgs = arguments
	return pgconn.CommandTag{}, nil
}

func TestPaymentCommandRepo_RecordCommandFailureTx_DeadLetter(t *testing.T) {
	repo := NewPaymentCommandRepo(nil)
	ctx := context.Background()

	t.Run("dead letters when attempt count reaches max attempts", func(t *testing.T) {
		tx := &mockCommandTx{}
		cmd := &domain.PaymentCommand{
			ID:           101,
			AttemptCount: 5,
			MaxAttempts:  5,
			CreatedAt:    time.Now().Add(-1 * time.Hour),
		}

		err := repo.RecordCommandFailureTx(ctx, tx, cmd, "gateway timeout", 10*time.Second)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cmd.Status != domain.PaymentCommandStatusDeadLetter {
			t.Fatalf("expected status dead_letter, got %s", cmd.Status)
		}
		if !strings.Contains(tx.lastSQL, "status = 'dead_letter'") {
			t.Fatalf("expected SQL to set status dead_letter, got: %s", tx.lastSQL)
		}
		if !strings.Contains(tx.lastSQL, "failed_at = now()") {
			t.Fatalf("expected SQL to set failed_at, got: %s", tx.lastSQL)
		}
	})

	t.Run("dead letters when age exceeds MaxPaymentCommandRetryAge even with low attempts", func(t *testing.T) {
		tx := &mockCommandTx{}
		cmd := &domain.PaymentCommand{
			ID:           102,
			AttemptCount: 1,
			MaxAttempts:  5,
			CreatedAt:    time.Now().Add(-49 * time.Hour), // Older than domain.MaxPaymentCommandRetryAge (48h)
		}

		err := repo.RecordCommandFailureTx(ctx, tx, cmd, "age expired failure", 10*time.Second)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cmd.Status != domain.PaymentCommandStatusDeadLetter {
			t.Fatalf("expected status dead_letter for expired age, got %s", cmd.Status)
		}
		if !strings.Contains(tx.lastSQL, "status = 'dead_letter'") {
			t.Fatalf("expected SQL to set status dead_letter, got: %s", tx.lastSQL)
		}
		if !strings.Contains(tx.lastSQL, "failed_at = now()") {
			t.Fatalf("expected SQL to set failed_at, got: %s", tx.lastSQL)
		}
	})

	t.Run("retries as pending when under both attempt limit and retry age", func(t *testing.T) {
		tx := &mockCommandTx{}
		cmd := &domain.PaymentCommand{
			ID:           103,
			AttemptCount: 2,
			MaxAttempts:  5,
			CreatedAt:    time.Now().Add(-2 * time.Hour),
		}

		err := repo.RecordCommandFailureTx(ctx, tx, cmd, "temporary connection error", 15*time.Second)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cmd.Status != domain.PaymentCommandStatusPending {
			t.Fatalf("expected status pending, got %s", cmd.Status)
		}
		if !strings.Contains(tx.lastSQL, "status = 'pending'") {
			t.Fatalf("expected SQL to set status pending, got: %s", tx.lastSQL)
		}
		if !strings.Contains(tx.lastSQL, "next_retry_at = $3") {
			t.Fatalf("expected SQL to schedule next_retry_at, got: %s", tx.lastSQL)
		}
	})
}
