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

type okIDRow struct{ id uuid.UUID }

func (r okIDRow) Scan(dest ...any) error {
	ptr := dest[0].(*uuid.UUID)
	*ptr = r.id
	return nil
}

type stubCreateOKDBTX struct{ id uuid.UUID }

func (s stubCreateOKDBTX) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (s stubCreateOKDBTX) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, nil
}
func (s stubCreateOKDBTX) QueryRow(context.Context, string, ...any) pgx.Row {
	return okIDRow{id: s.id}
}

func TestPaymentRepoCreate_BankMatchPreservesUPITxnIDAndProviderBank(t *testing.T) {
	propID := uuid.New()
	dueID := uuid.New()
	tenantID := uuid.New()
	utr := "UTR_BANK_MATCH_1"

	repo := NewPaymentRepo(stubCreateOKDBTX{id: uuid.New()})
	p := &domain.Payment{
		PropertyID: &propID,
		DueID:      dueID,
		TenantID:   tenantID,
		UPITxnID:   &utr,
		Amount:     5000,
		MatchedBy:  domain.MatchedByDueCode,
	}
	if err := repo.Create(context.Background(), p); err != nil {
		t.Fatalf("expected bank match Create to succeed, got %v", err)
	}
	if p.Provider != "bank" {
		t.Fatalf("expected Provider 'bank', got %q", p.Provider)
	}
	if p.UPITxnID == nil || *p.UPITxnID != utr {
		t.Fatalf("expected UPITxnID %q preserved for bank match, got %v", utr, p.UPITxnID)
	}
}

func TestPaymentRepoCreate_RejectsUnknownMatchedBy(t *testing.T) {
	propID := uuid.New()
	dueID := uuid.New()
	tenantID := uuid.New()

	repo := NewPaymentRepo(stubCreateOKDBTX{id: uuid.New()})
	p := &domain.Payment{
		PropertyID: &propID,
		DueID:      dueID,
		TenantID:   tenantID,
		Amount:     5000,
		MatchedBy:  domain.MatchedBy("unknown_source"),
	}
	if err := repo.Create(context.Background(), p); err == nil {
		t.Fatal("expected error for unknown matched_by without explicit provider")
	}
}
