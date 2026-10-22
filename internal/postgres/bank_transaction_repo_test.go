package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockBankTxnRow struct {
	scanFn func(dest ...any) error
}

func (r *mockBankTxnRow) Scan(dest ...any) error {
	if r.scanFn != nil {
		return r.scanFn(dest...)
	}
	return nil
}

type mockBankTxnRows struct {
	items []domain.BankTransaction
	idx   int
	err   error
}

func (r *mockBankTxnRows) Close() {}

func (r *mockBankTxnRows) Err() error {
	return r.err
}

func (r *mockBankTxnRows) CommandTag() pgconn.CommandTag {
	return pgconn.CommandTag{}
}

func (r *mockBankTxnRows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}

func (r *mockBankTxnRows) Next() bool {
	if r.idx < len(r.items) {
		r.idx++
		return true
	}
	return false
}

func (r *mockBankTxnRows) Scan(dest ...any) error {
	if r.idx <= 0 || r.idx > len(r.items) {
		return errors.New("out of bounds")
	}
	item := r.items[r.idx-1]
	if len(dest) >= 25 {
		*dest[0].(*uuid.UUID) = item.ID
		*dest[1].(*uuid.UUID) = item.PropertyID
		*dest[2].(**uuid.UUID) = item.BankAccountID
		*dest[3].(*string) = item.TxnID
		*dest[4].(*int64) = item.AmountPaise
		*dest[5].(*string) = item.RowType
		*dest[6].(*time.Time) = item.TxnDate
		*dest[7].(*string) = item.Narration
		*dest[8].(**int64) = item.ClosingBalancePaise
		*dest[9].(*int) = item.OccurrenceIndex
		*dest[10].(*string) = item.DedupHash
		*dest[11].(*string) = string(item.Status)
		*dest[12].(*string) = item.Classification
		*dest[13].(*bool) = item.IsReversal
		*dest[14].(*bool) = item.IsInternalTransfer
		*dest[15].(*string) = item.PayerPhone
		*dest[16].(*string) = item.PayerVPA
		*dest[17].(**uuid.UUID) = item.MatchedDueID
		*dest[18].(**uuid.UUID) = item.SuggestedDueID
		*dest[19].(*float64) = item.ConfidenceScore
		*dest[20].(**time.Time) = item.MatchedAt
		*dest[21].(**uuid.UUID) = item.MatchedBy
		*dest[22].(**uuid.UUID) = item.JournalEntryID
		*dest[23].(*time.Time) = item.CreatedAt
		*dest[24].(*time.Time) = item.UpdatedAt
	}
	return nil
}

func (r *mockBankTxnRows) Values() ([]any, error) {
	return nil, nil
}

func (r *mockBankTxnRows) RawValues() [][]byte {
	return nil
}

func (r *mockBankTxnRows) Conn() *pgx.Conn {
	return nil
}

type mockBankTxnDB struct {
	queryRowFn func(ctx context.Context, sql string, args ...any) pgx.Row
	queryFn    func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	execFn     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (m *mockBankTxnDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if m.execFn != nil {
		return m.execFn(ctx, sql, args...)
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (m *mockBankTxnDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if m.queryFn != nil {
		return m.queryFn(ctx, sql, args...)
	}
	return &mockBankTxnRows{}, nil
}

func (m *mockBankTxnDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if m.queryRowFn != nil {
		return m.queryRowFn(ctx, sql, args...)
	}
	return &mockBankTxnRow{}
}

func TestBankTransactionRepo_InsertTransaction(t *testing.T) {
	propID := uuid.New()
	txnID := uuid.New()
	now := time.Now().UTC()

	tests := []struct {
		name         string
		scanErr      error
		wantInserted bool
		wantErr      bool
	}{
		{
			name:         "New transaction inserted",
			scanErr:      nil,
			wantInserted: true,
			wantErr:      false,
		},
		{
			name:         "Duplicate dedup hash safely skipped",
			scanErr:      pgx.ErrNoRows,
			wantInserted: false,
			wantErr:      false,
		},
		{
			name:         "Database execution failure",
			scanErr:      errors.New("connection failed"),
			wantInserted: false,
			wantErr:      true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &mockBankTxnDB{
				queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
					return &mockBankTxnRow{
						scanFn: func(dest ...any) error {
							if tc.scanErr != nil {
								return tc.scanErr
							}
							if len(dest) >= 3 {
								*dest[0].(*uuid.UUID) = txnID
								*dest[1].(*time.Time) = now
								*dest[2].(*time.Time) = now
							}
							return nil
						},
					}
				},
			}
			repo := NewBankTransactionRepo(db)
			txn := &domain.BankTransaction{
				PropertyID:  propID,
				AmountPaise: 100000,
				TxnDate:     now,
				Narration:   "Rent payment",
			}
			inserted, err := repo.InsertTransaction(context.Background(), nil, txn)
			if (err != nil) != tc.wantErr {
				t.Fatalf("InsertTransaction() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if inserted != tc.wantInserted {
				t.Errorf("InsertTransaction() inserted = %v, wantInserted = %v", inserted, tc.wantInserted)
			}
		})
	}
}

func TestBankTransactionRepo_GetByID(t *testing.T) {
	txnID := uuid.New()
	propID := uuid.New()
	now := time.Now().UTC()

	tests := []struct {
		name         string
		scanErr      error
		wantErr      bool
		wantedErrIso error
	}{
		{
			name:    "Transaction found",
			scanErr: nil,
			wantErr: false,
		},
		{
			name:         "Transaction not found",
			scanErr:      pgx.ErrNoRows,
			wantErr:      true,
			wantedErrIso: ErrBankTransactionNotFound,
		},
		{
			name:    "Database error",
			scanErr: errors.New("db query error"),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &mockBankTxnDB{
				queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
					return &mockBankTxnRow{
						scanFn: func(dest ...any) error {
							if tc.scanErr != nil {
								return tc.scanErr
							}
							if len(dest) >= 25 {
								*dest[0].(*uuid.UUID) = txnID
								*dest[1].(*uuid.UUID) = propID
								*dest[2].(**uuid.UUID) = nil
								*dest[3].(*string) = "TXN123"
								*dest[4].(*int64) = 500000
								*dest[5].(*string) = "credit"
								*dest[6].(*time.Time) = now
								*dest[7].(*string) = "UPI credit"
								*dest[8].(**int64) = nil
								*dest[9].(*int) = 1
								*dest[10].(*string) = "hash123"
								*dest[11].(*string) = "unmatched"
								*dest[12].(*string) = "rent"
								*dest[13].(*bool) = false
								*dest[14].(*bool) = false
								*dest[15].(*string) = ""
								*dest[16].(*string) = ""
								*dest[17].(**uuid.UUID) = nil
								*dest[18].(**uuid.UUID) = nil
								*dest[19].(*float64) = 0.95
								*dest[20].(**time.Time) = nil
								*dest[21].(**uuid.UUID) = nil
								*dest[22].(**uuid.UUID) = nil
								*dest[23].(*time.Time) = now
								*dest[24].(*time.Time) = now
							}
							return nil
						},
					}
				},
			}
			repo := NewBankTransactionRepo(db)
			txn, err := repo.GetByID(context.Background(), txnID)
			if (err != nil) != tc.wantErr {
				t.Fatalf("GetByID() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantedErrIso != nil && !errors.Is(err, tc.wantedErrIso) {
				t.Errorf("expected error %v, got %v", tc.wantedErrIso, err)
			}
			if !tc.wantErr && (txn == nil || txn.ID != txnID || txn.AmountPaise != 500000) {
				t.Errorf("unexpected transaction result: %+v", txn)
			}
		})
	}
}

func TestBankTransactionRepo_UpdateStatus(t *testing.T) {
	txnID := uuid.New()
	dueID := uuid.New()
	now := time.Now().UTC()

	tests := []struct {
		name    string
		execErr error
		wantErr bool
	}{
		{
			name:    "Successful status update",
			execErr: nil,
			wantErr: false,
		},
		{
			name:    "Database failure",
			execErr: errors.New("exec error"),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &mockBankTxnDB{
				execFn: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
					return pgconn.NewCommandTag("UPDATE 1"), tc.execErr
				},
			}
			repo := NewBankTransactionRepo(db)
			err := repo.UpdateStatus(context.Background(), nil, txnID, domain.BankTxnMatched, &dueID, nil, &now, nil)
			if (err != nil) != tc.wantErr {
				t.Fatalf("UpdateStatus() error = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}
