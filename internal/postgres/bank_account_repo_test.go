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

type mockBankAccountRow struct {
	scanFn func(dest ...any) error
}

func (r *mockBankAccountRow) Scan(dest ...any) error {
	if r.scanFn != nil {
		return r.scanFn(dest...)
	}
	return nil
}

type mockBankAccountRows struct {
	items []domain.BankAccount
	idx   int
	err   error
}

func (r *mockBankAccountRows) Close() {}

func (r *mockBankAccountRows) Err() error {
	return r.err
}

func (r *mockBankAccountRows) CommandTag() pgconn.CommandTag {
	return pgconn.CommandTag{}
}

func (r *mockBankAccountRows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}

func (r *mockBankAccountRows) Next() bool {
	if r.idx < len(r.items) {
		r.idx++
		return true
	}
	return false
}

func (r *mockBankAccountRows) Scan(dest ...any) error {
	if r.idx <= 0 || r.idx > len(r.items) {
		return errors.New("out of bounds")
	}
	item := r.items[r.idx-1]
	if len(dest) >= 10 {
		*dest[0].(*uuid.UUID) = item.ID
		*dest[1].(*uuid.UUID) = item.PropertyID
		*dest[2].(*string) = item.BankName
		*dest[3].(*string) = string(item.AccountType)
		*dest[4].(*string) = item.AccountNumberLast4
		*dest[5].(*string) = item.Label
		*dest[6].(*string) = item.StatementProfile
		*dest[7].(*bool) = item.IsActive
		*dest[8].(*time.Time) = item.CreatedAt
		*dest[9].(*time.Time) = item.UpdatedAt
	}
	return nil
}

func (r *mockBankAccountRows) Values() ([]any, error) {
	return nil, nil
}

func (r *mockBankAccountRows) RawValues() [][]byte {
	return nil
}

func (r *mockBankAccountRows) Conn() *pgx.Conn {
	return nil
}

type mockBankAccountDB struct {
	queryRowFn func(ctx context.Context, sql string, args ...any) pgx.Row
	queryFn    func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	execFn     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (m *mockBankAccountDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if m.execFn != nil {
		return m.execFn(ctx, sql, args...)
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (m *mockBankAccountDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if m.queryFn != nil {
		return m.queryFn(ctx, sql, args...)
	}
	return &mockBankAccountRows{}, nil
}

func (m *mockBankAccountDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if m.queryRowFn != nil {
		return m.queryRowFn(ctx, sql, args...)
	}
	return &mockBankAccountRow{}
}

func TestBankAccountRepo_Create(t *testing.T) {
	propID := uuid.New()
	now := time.Now().UTC()

	tests := []struct {
		name    string
		scanErr error
		wantErr bool
	}{
		{
			name:    "Successful creation",
			scanErr: nil,
			wantErr: false,
		},
		{
			name:    "Database failure",
			scanErr: errors.New("unique violation"),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &mockBankAccountDB{
				queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
					return &mockBankAccountRow{
						scanFn: func(dest ...any) error {
							if tc.scanErr != nil {
								return tc.scanErr
							}
							if len(dest) >= 2 {
								*dest[0].(*time.Time) = now
								*dest[1].(*time.Time) = now
							}
							return nil
						},
					}
				},
			}
			repo := NewBankAccountRepo(db)
			acct := &domain.BankAccount{
				PropertyID:         propID,
				BankName:           "HDFC Bank",
				AccountNumberLast4: "1234",
			}
			err := repo.Create(context.Background(), acct)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Create() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && acct.AccountType != domain.BankAccountTypeSavings {
				t.Errorf("expected default savings account type, got %s", acct.AccountType)
			}
		})
	}
}

func TestBankAccountRepo_GetByID(t *testing.T) {
	acctID := uuid.New()
	propID := uuid.New()
	now := time.Now().UTC()

	tests := []struct {
		name         string
		scanErr      error
		wantErr      bool
		wantedErrIso error
	}{
		{
			name:    "Account found",
			scanErr: nil,
			wantErr: false,
		},
		{
			name:         "Account not found",
			scanErr:      pgx.ErrNoRows,
			wantErr:      true,
			wantedErrIso: ErrBankAccountNotFound,
		},
		{
			name:    "Query error",
			scanErr: errors.New("db error"),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &mockBankAccountDB{
				queryRowFn: func(_ context.Context, _ string, _ ...any) pgx.Row {
					return &mockBankAccountRow{
						scanFn: func(dest ...any) error {
							if tc.scanErr != nil {
								return tc.scanErr
							}
							if len(dest) >= 10 {
								*dest[0].(*uuid.UUID) = acctID
								*dest[1].(*uuid.UUID) = propID
								*dest[2].(*string) = "SBI"
								*dest[3].(*string) = "current"
								*dest[4].(*string) = "5678"
								*dest[5].(*string) = "SBI Current"
								*dest[6].(*string) = "generic"
								*dest[7].(*bool) = true
								*dest[8].(*time.Time) = now
								*dest[9].(*time.Time) = now
							}
							return nil
						},
					}
				},
			}
			repo := NewBankAccountRepo(db)
			acct, err := repo.GetByID(context.Background(), acctID)
			if (err != nil) != tc.wantErr {
				t.Fatalf("GetByID() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantedErrIso != nil && !errors.Is(err, tc.wantedErrIso) {
				t.Errorf("expected error %v, got %v", tc.wantedErrIso, err)
			}
			if !tc.wantErr && (acct == nil || acct.ID != acctID || acct.AccountType != domain.BankAccountTypeCurrent) {
				t.Errorf("unexpected account result: %+v", acct)
			}
		})
	}
}

func TestBankAccountRepo_ListByProperty(t *testing.T) {
	propID := uuid.New()
	acctID1 := uuid.New()
	acctID2 := uuid.New()
	now := time.Now().UTC()

	tests := []struct {
		name      string
		items     []domain.BankAccount
		queryErr  error
		wantErr   bool
		wantCount int
	}{
		{
			name: "Returns active accounts",
			items: []domain.BankAccount{
				{ID: acctID1, PropertyID: propID, BankName: "HDFC", AccountType: "savings", AccountNumberLast4: "1111", IsActive: true, CreatedAt: now, UpdatedAt: now},
				{ID: acctID2, PropertyID: propID, BankName: "ICICI", AccountType: "current", AccountNumberLast4: "2222", IsActive: true, CreatedAt: now, UpdatedAt: now},
			},
			wantErr:   false,
			wantCount: 2,
		},
		{
			name:      "Empty accounts list",
			items:     []domain.BankAccount{},
			wantErr:   false,
			wantCount: 0,
		},
		{
			name:     "Query execution error",
			queryErr: errors.New("db query error"),
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &mockBankAccountDB{
				queryFn: func(_ context.Context, _ string, _ ...any) (pgx.Rows, error) {
					if tc.queryErr != nil {
						return nil, tc.queryErr
					}
					return &mockBankAccountRows{items: tc.items}, nil
				},
			}
			repo := NewBankAccountRepo(db)
			res, err := repo.ListByProperty(context.Background(), propID)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ListByProperty() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if !tc.wantErr && len(res) != tc.wantCount {
				t.Errorf("expected %d accounts, got %d", tc.wantCount, len(res))
			}
		})
	}
}

func TestBankAccountRepo_Deactivate(t *testing.T) {
	acctID := uuid.New()

	tests := []struct {
		name         string
		rowsAffected int64
		execErr      error
		wantErr      bool
		wantedErrIso error
	}{
		{
			name:         "Account successfully deactivated",
			rowsAffected: 1,
			execErr:      nil,
			wantErr:      false,
		},
		{
			name:         "Account not found when deactivating",
			rowsAffected: 0,
			execErr:      nil,
			wantErr:      true,
			wantedErrIso: ErrBankAccountNotFound,
		},
		{
			name:    "Database failure",
			execErr: errors.New("db failure"),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &mockBankAccountDB{
				execFn: func(_ context.Context, _ string, _ ...any) (pgconn.CommandTag, error) {
					if tc.execErr != nil {
						return pgconn.CommandTag{}, tc.execErr
					}
					if tc.rowsAffected > 0 {
						return pgconn.NewCommandTag("UPDATE 1"), nil
					}
					return pgconn.NewCommandTag("UPDATE 0"), nil
				},
			}
			repo := NewBankAccountRepo(db)
			err := repo.Deactivate(context.Background(), acctID)
			if (err != nil) != tc.wantErr {
				t.Fatalf("Deactivate() error = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantedErrIso != nil && !errors.Is(err, tc.wantedErrIso) {
				t.Errorf("expected error %v, got %v", tc.wantedErrIso, err)
			}
		})
	}
}
