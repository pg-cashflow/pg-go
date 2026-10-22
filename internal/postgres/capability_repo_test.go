package postgres

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// mockCapabilityRow implements pgx.Row for boolean queries.
type mockCapabilityRow struct {
	val bool
	err error
}

func (r *mockCapabilityRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) > 0 {
		if ptr, ok := dest[0].(*bool); ok {
			*ptr = r.val
			return nil
		}
	}
	return nil
}

// mockCapabilityRows implements pgx.Rows for string slice queries.
type mockCapabilityRows struct {
	items []string
	idx   int
	err   error
}

func (r *mockCapabilityRows) Close() {}

func (r *mockCapabilityRows) Err() error {
	return r.err
}

func (r *mockCapabilityRows) CommandTag() pgconn.CommandTag {
	return pgconn.CommandTag{}
}

func (r *mockCapabilityRows) FieldDescriptions() []pgconn.FieldDescription {
	return nil
}

func (r *mockCapabilityRows) Next() bool {
	if r.idx < len(r.items) {
		r.idx++
		return true
	}
	return false
}

func (r *mockCapabilityRows) Scan(dest ...any) error {
	if len(dest) > 0 {
		if ptr, ok := dest[0].(*string); ok {
			*ptr = r.items[r.idx-1]
			return nil
		}
	}
	return nil
}

func (r *mockCapabilityRows) Values() ([]any, error) {
	return nil, nil
}

func (r *mockCapabilityRows) RawValues() [][]byte {
	return nil
}

func (r *mockCapabilityRows) Conn() *pgx.Conn {
	return nil
}

// mockCapabilityDB implements DBTX for CapabilityRepo unit testing.
type mockCapabilityDB struct {
	queryRowFn func(ctx context.Context, sql string, args ...any) pgx.Row
	queryFn    func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	execFn     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (m *mockCapabilityDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if m.execFn != nil {
		return m.execFn(ctx, sql, args...)
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (m *mockCapabilityDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if m.queryFn != nil {
		return m.queryFn(ctx, sql, args...)
	}
	return &mockCapabilityRows{}, nil
}

func (m *mockCapabilityDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if m.queryRowFn != nil {
		return m.queryRowFn(ctx, sql, args...)
	}
	return &mockCapabilityRow{}
}

func TestCapabilityRepo_HasActive(t *testing.T) {
	propID := uuid.New()
	userID := uuid.New()
	queryErr := errors.New("database timeout")

	tests := []struct {
		name       string
		mockRow    *mockCapabilityRow
		capability string
		wantOK     bool
		wantErr    bool
	}{
		{
			name:       "Active grant exists",
			mockRow:    &mockCapabilityRow{val: true},
			capability: "dues.view",
			wantOK:     true,
			wantErr:    false,
		},
		{
			name:       "Grant missing - zero rows must fail closed",
			mockRow:    &mockCapabilityRow{val: false},
			capability: "dues.view",
			wantOK:     false,
			wantErr:    false,
		},
		{
			name:       "Database query error fails closed",
			mockRow:    &mockCapabilityRow{err: queryErr},
			capability: "dues.view",
			wantOK:     false,
			wantErr:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &mockCapabilityDB{
				queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
					return tc.mockRow
				},
			}
			repo := NewCapabilityRepo(db)
			ok, err := repo.HasActive(context.Background(), propID, userID, tc.capability)

			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ok != tc.wantOK {
				t.Fatalf("expected ok=%v, got %v", tc.wantOK, ok)
			}
		})
	}
}

func TestCapabilityRepo_ListActive(t *testing.T) {
	propID := uuid.New()
	userID := uuid.New()
	queryErr := errors.New("connection failed")

	tests := []struct {
		name     string
		queryFn  func(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
		wantCaps []string
		wantErr  bool
	}{
		{
			name: "Active grants exist",
			queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
				return &mockCapabilityRows{items: []string{"dues.view", "tenants.view"}}, nil
			},
			wantCaps: []string{"dues.view", "tenants.view"},
			wantErr:  false,
		},
		{
			name: "Zero grants - must return empty slice without fallback fabrication",
			queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
				return &mockCapabilityRows{items: nil}, nil
			},
			wantCaps: []string{},
			wantErr:  false,
		},
		{
			name: "Database query error",
			queryFn: func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
				return nil, queryErr
			},
			wantCaps: nil,
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &mockCapabilityDB{queryFn: tc.queryFn}
			repo := NewCapabilityRepo(db)
			caps, err := repo.ListActive(context.Background(), propID, userID)

			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(caps, tc.wantCaps) {
				t.Fatalf("expected caps=%v, got %v", tc.wantCaps, caps)
			}
		})
	}
}

func TestCapabilityRepo_Grant(t *testing.T) {
	propID := uuid.New()
	userID := uuid.New()
	grantedBy := uuid.New()
	execErr := errors.New("unique constraint violation")

	tests := []struct {
		name    string
		execErr error
		wantErr bool
	}{
		{
			name:    "Successful grant",
			execErr: nil,
			wantErr: false,
		},
		{
			name:    "Database execution failure",
			execErr: execErr,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &mockCapabilityDB{
				execFn: func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
					return pgconn.NewCommandTag("INSERT 1"), tc.execErr
				},
			}
			repo := NewCapabilityRepo(db)
			err := repo.Grant(context.Background(), propID, userID, "dues.view", grantedBy)

			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCapabilityRepo_Revoke(t *testing.T) {
	propID := uuid.New()
	userID := uuid.New()
	execErr := errors.New("db error")

	tests := []struct {
		name    string
		execErr error
		wantErr bool
	}{
		{
			name:    "Successful revoke",
			execErr: nil,
			wantErr: false,
		},
		{
			name:    "Database failure",
			execErr: execErr,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &mockCapabilityDB{
				execFn: func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
					return pgconn.NewCommandTag("UPDATE 1"), tc.execErr
				},
			}
			repo := NewCapabilityRepo(db)
			err := repo.Revoke(context.Background(), propID, userID, "dues.view")

			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCapabilityRepo_RevokeAll(t *testing.T) {
	propID := uuid.New()
	userID := uuid.New()
	execErr := errors.New("db error")

	tests := []struct {
		name    string
		execErr error
		wantErr bool
	}{
		{
			name:    "Successful revoke all",
			execErr: nil,
			wantErr: false,
		},
		{
			name:    "Database failure",
			execErr: execErr,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &mockCapabilityDB{
				execFn: func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
					return pgconn.NewCommandTag("UPDATE 5"), tc.execErr
				},
			}
			repo := NewCapabilityRepo(db)
			err := repo.RevokeAll(context.Background(), propID, userID)

			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
