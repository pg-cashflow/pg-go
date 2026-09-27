package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockIntentRow struct {
	id uuid.UUID
}

func (m *mockIntentRow) Scan(dest ...any) error {
	if len(dest) > 0 {
		if ptr, ok := dest[0].(*uuid.UUID); ok {
			*ptr = m.id
			return nil
		}
	}
	return nil
}

type mockFailingDBTX struct {
	execCount  int
	failOnExec int
}

func (m *mockFailingDBTX) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	m.execCount++
	if m.execCount == m.failOnExec {
		return pgconn.CommandTag{}, errors.New("simulated exec failure on second due")
	}
	return pgconn.CommandTag{}, nil
}

func (m *mockFailingDBTX) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, nil
}

func (m *mockFailingDBTX) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return &mockIntentRow{id: uuid.New()}
}

func TestPaymentIntentRepo_CreateWithDues_OuterTxErrorPropagation(t *testing.T) {
	mockDB := &mockFailingDBTX{failOnExec: 2} // fail on second due insert
	repo := NewPaymentIntentRepo(mockDB)

	intent := &domain.PaymentIntent{
		AmountPaise: 50000,
	}
	due1 := uuid.New()
	due2 := uuid.New()

	err := repo.CreateWithDues(context.Background(), intent, []uuid.UUID{due1, due2}, []int64{25000, 25000})
	if err == nil {
		t.Fatal("expected error on failing second due insert, got nil")
	}
	if err.Error() != "simulated exec failure on second due" {
		t.Fatalf("unexpected error: %v", err)
	}
	if mockDB.execCount != 2 {
		t.Fatalf("expected exactly 2 exec calls before stopping, got %d", mockDB.execCount)
	}
}
