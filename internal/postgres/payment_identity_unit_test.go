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

type errRow struct{ err error }

func (r errRow) Scan(_ ...any) error { return r.err }

type stubCreateErrDBTX struct {
	err error
}

func (s stubCreateErrDBTX) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func (s stubCreateErrDBTX) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, nil
}

func (s stubCreateErrDBTX) QueryRow(context.Context, string, ...any) pgx.Row {
	return errRow{err: s.err}
}

// F1 regression: PaymentRepo.Create must preserve pgconn.PgError (including ConstraintName)
// when wrapping SQLSTATE 23505 as domain.ErrDuplicateTxn, so callers can distinguish
// uq_payments_cf_payment_id from unrelated unique-constraint collisions.
func TestPaymentRepoCreate_PreservesPgErrorConstraintName(t *testing.T) {
	propID := uuid.New()
	dueID := uuid.New()
	tenantID := uuid.New()
	cfID := "cf_pay_f1_test"

	pgErr := &pgconn.PgError{
		Code:           "23505",
		ConstraintName: "uq_payments_legacy_utr",
		Message:        "duplicate key value violates unique constraint \"uq_payments_legacy_utr\"",
	}
	repo := NewPaymentRepo(stubCreateErrDBTX{err: pgErr})
	p := &domain.Payment{
		PropertyID:  &propID,
		DueID:       dueID,
		TenantID:    tenantID,
		CFPaymentID: &cfID,
		Provider:    "cashfree",
		Amount:      10000,
		MatchedBy:   domain.MatchedByCashfree,
	}

	err := repo.Create(context.Background(), p)
	if !errors.Is(err, domain.ErrDuplicateTxn) {
		t.Fatalf("expected errors.Is(err, domain.ErrDuplicateTxn), got %v", err)
	}
	var gotPgErr *pgconn.PgError
	if !errors.As(err, &gotPgErr) {
		t.Fatalf("expected errors.As(err, &pgErr) to succeed and preserve ConstraintName, got %T: %v", err, err)
	}
	if gotPgErr.ConstraintName != "uq_payments_legacy_utr" {
		t.Fatalf("expected ConstraintName uq_payments_legacy_utr, got %q", gotPgErr.ConstraintName)
	}
}
