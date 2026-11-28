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

type scriptedRows struct {
	ids       []uuid.UUID
	scanErrAt int // 1-based row index to fail Scan, 0 = none
	rowsErr   error
	idx       int
}

func (r *scriptedRows) Close()                        {}
func (r *scriptedRows) Err() error                    { return r.rowsErr }
func (r *scriptedRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }
func (r *scriptedRows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}
func (r *scriptedRows) Next() bool {
	if r.idx >= len(r.ids) {
		return false
	}
	r.idx++
	return true
}
func (r *scriptedRows) Scan(dest ...any) error {
	if r.scanErrAt > 0 && r.idx == r.scanErrAt {
		return errors.New("injected scan error")
	}
	id := r.ids[r.idx-1]
	ptr := dest[0].(**uuid.UUID)
	*ptr = &id
	return nil
}
func (r *scriptedRows) Values() ([]any, error) { return nil, nil }
func (r *scriptedRows) RawValues() [][]byte    { return nil }
func (r *scriptedRows) Conn() *pgx.Conn        { return nil }

type scriptedReconcileDBTX struct {
	results []struct {
		rows pgx.Rows
		err  error
	}
	call int
}

func (s *scriptedReconcileDBTX) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}
func (s *scriptedReconcileDBTX) QueryRow(context.Context, string, ...any) pgx.Row {
	return errRow{err: errors.New("unexpected QueryRow")}
}
func (s *scriptedReconcileDBTX) Query(context.Context, string, ...any) (pgx.Rows, error) {
	if s.call >= len(s.results) {
		return nil, errors.New("unexpected Query call")
	}
	res := s.results[s.call]
	s.call++
	return res.rows, res.err
}

func TestCollectRefundReconcileDueIDs_FailClosedAndDeduplicate(t *testing.T) {
	refID := uuid.New()
	payID := uuid.New()
	duePrimary := uuid.New()
	due1 := uuid.New()
	due2 := uuid.New()

	tests := []struct {
		name      string
		refundID  uuid.UUID
		paymentID uuid.UUID
		primary   uuid.UUID
		results   []struct {
			rows pgx.Rows
			err  error
		}
		wantErr bool
		wantLen int
	}{
		{
			name:      "nil refund id",
			refundID:  uuid.Nil,
			paymentID: payID,
			wantErr:   true,
		},
		{
			name:      "refund_allocations query fails",
			refundID:  refID,
			paymentID: payID,
			primary:   duePrimary,
			results: []struct {
				rows pgx.Rows
				err  error
			}{
				{err: errors.New("db query fail")},
			},
			wantErr: true,
		},
		{
			name:      "payment_allocations query fails",
			refundID:  refID,
			paymentID: payID,
			primary:   duePrimary,
			results: []struct {
				rows pgx.Rows
				err  error
			}{
				{rows: &scriptedRows{ids: []uuid.UUID{due1}}},
				{err: errors.New("payment alloc query fail")},
			},
			wantErr: true,
		},
		{
			name:      "first row scan fails",
			refundID:  refID,
			paymentID: payID,
			results: []struct {
				rows pgx.Rows
				err  error
			}{
				{rows: &scriptedRows{ids: []uuid.UUID{due1, due2}, scanErrAt: 1}},
			},
			wantErr: true,
		},
		{
			name:      "later row scan fails on payment_allocations",
			refundID:  refID,
			paymentID: payID,
			results: []struct {
				rows pgx.Rows
				err  error
			}{
				{rows: &scriptedRows{ids: []uuid.UUID{due1}}},
				{rows: &scriptedRows{ids: []uuid.UUID{due1, due2}, scanErrAt: 2}},
			},
			wantErr: true,
		},
		{
			name:      "first rows.Err fails",
			refundID:  refID,
			paymentID: payID,
			results: []struct {
				rows pgx.Rows
				err  error
			}{
				{rows: &scriptedRows{ids: []uuid.UUID{due1}, rowsErr: errors.New("stream truncated")}},
			},
			wantErr: true,
		},
		{
			name:      "second rows.Err fails",
			refundID:  refID,
			paymentID: payID,
			results: []struct {
				rows pgx.Rows
				err  error
			}{
				{rows: &scriptedRows{ids: []uuid.UUID{due1}}},
				{rows: &scriptedRows{ids: []uuid.UUID{due2}, rowsErr: errors.New("stream truncated 2")}},
			},
			wantErr: true,
		},
		{
			name:      "duplicate due IDs deduplicated with primary",
			refundID:  refID,
			paymentID: payID,
			primary:   duePrimary,
			results: []struct {
				rows pgx.Rows
				err  error
			}{
				{rows: &scriptedRows{ids: []uuid.UUID{duePrimary, due1}}},
				{rows: &scriptedRows{ids: []uuid.UUID{due1, due2}}},
			},
			wantErr: false,
			wantLen: 3,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &scriptedReconcileDBTX{results: tc.results}
			got, err := collectRefundReconcileDueIDs(context.Background(), db, tc.refundID, tc.primary, tc.paymentID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (ids=%v)", got)
				}
				if got != nil {
					t.Fatalf("expected nil slice on error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != tc.wantLen {
				t.Fatalf("expected %d unique due IDs, got %d (%v)", tc.wantLen, len(got), got)
			}
		})
	}
}

func TestReconcileRefundTransition_RejectsNilIdentity(t *testing.T) {
	repo := NewPaymentRepo(stubCreateErrDBTX{})
	if err := repo.ReconcileRefundTransition(context.Background(), nil, domain.RefundStatusSucceeded); err == nil {
		t.Fatal("expected error for nil refund")
	}
	if err := repo.ReconcileRefundTransition(context.Background(), &domain.GatewayRefund{ID: uuid.New()}, domain.RefundStatusSucceeded); err == nil {
		t.Fatal("expected error for nil payment_id")
	}
}
