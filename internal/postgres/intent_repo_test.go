package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
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

func TestPoolFromDB_UnwrapsScopedDB(t *testing.T) {
	scoped := &ScopedDB{pool: nil}
	if pool := PoolFromDB(scoped); pool != nil {
		t.Fatal("expected nil pool from empty ScopedDB")
	}
}

type mockVacatedTenantDBTX struct {
	mockFailingDBTX
}

func (m *mockVacatedTenantDBTX) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return &mockStatusRow{status: domain.TenantStatusVacated}
}

type mockStatusRow struct {
	status domain.TenantStatus
}

func (m *mockStatusRow) Scan(dest ...any) error {
	if len(dest) > 0 {
		if ptr, ok := dest[0].(*domain.TenantStatus); ok {
			*ptr = m.status
			return nil
		}
	}
	return nil
}

func TestPaymentIntentRepo_ClaimInitiatingIntent_RequiresPropertyScope(t *testing.T) {
	mockDB := &mockVacatedTenantDBTX{}
	repo := NewPaymentIntentRepo(mockDB)
	tenantID := uuid.New()
	dueID := uuid.New()
	_, _, _, err := repo.ClaimInitiatingIntent(context.Background(), tenantID, []uuid.UUID{dueID}, []int64{1000}, 1000, "ord-1", 10)
	if err == nil || err.Error() != "payment intent claim requires a property-scoped request context" {
		t.Fatalf("expected property-scope error, got %v", err)
	}
}

func TestPaymentIntentRepo_ClaimInitiatingIntent_RequiresOwnedTransaction(t *testing.T) {
	mockDB := &mockVacatedTenantDBTX{}
	repo := NewPaymentIntentRepo(mockDB)
	tenantID := uuid.New()
	dueID := uuid.New()
	ctx := requestscope.WithPropertyID(context.Background(), uuid.New())
	_, _, _, err := repo.ClaimInitiatingIntent(ctx, tenantID, []uuid.UUID{dueID}, []int64{1000}, 1000, "ord-1", 10)
	if err == nil || err.Error() != "payment intent claim requires a pool-backed database handle" {
		t.Fatalf("expected transaction-capability error, got %v", err)
	}
}

func TestPaymentIntentRepo_ClaimInitiatingIntent_RejectsInvalidAmountSnapshot(t *testing.T) {
	repo := NewPaymentIntentRepo(&mockVacatedTenantDBTX{})
	ctx := requestscope.WithPropertyID(context.Background(), uuid.New())
	tenantID := uuid.New()
	dueID := uuid.New()
	_, _, _, err := repo.ClaimInitiatingIntent(ctx, tenantID, []uuid.UUID{dueID}, []int64{1000}, 999, "ord-1", 10)
	if err == nil || err.Error() != "payment intent claim total mismatch: amounts sum to 1000 paise, total is 999 paise" {
		t.Fatalf("expected total mismatch error, got %v", err)
	}
	_, _, _, err = repo.ClaimInitiatingIntent(ctx, tenantID, []uuid.UUID{dueID, dueID}, []int64{500, 500}, 1000, "ord-2", 10)
	if err == nil || !strings.Contains(err.Error(), "duplicate due") {
		t.Fatalf("expected duplicate due error, got %v", err)
	}
}
