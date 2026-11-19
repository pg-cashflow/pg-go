package postgres

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

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

// mockUserRow implements pgx.Row for user locking queries.
type mockUserRow struct {
	role       string
	propID     *uuid.UUID
	disabledAt *time.Time
	err        error
}

func (r *mockUserRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) >= 2 {
		if ptrRole, ok := dest[0].(*string); ok {
			*ptrRole = r.role
		}
		if ptrProp, ok := dest[1].(**uuid.UUID); ok {
			*ptrProp = r.propID
		}
		if len(dest) >= 3 {
			if ptrDisabled, ok := dest[2].(**time.Time); ok {
				*ptrDisabled = r.disabledAt
			}
		}
	} else if len(dest) == 1 {
		if ptrID, ok := dest[0].(*uuid.UUID); ok && r.propID != nil {
			*ptrID = *r.propID
		}
	}
	return nil
}

// mockIntRow implements pgx.Row for single int queries (SELECT 1).
type mockIntRow struct {
	val int
	err error
}

func (r *mockIntRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) > 0 {
		if ptr, ok := dest[0].(*int); ok {
			*ptr = r.val
		}
	}
	return nil
}

func TestAssertCapabilityTx(t *testing.T) {
	propID := uuid.New()
	userID := uuid.New()
	otherPropID := uuid.New()

	t.Run("Owner bypass succeeds without DB query", func(t *testing.T) {
		db := &mockCapabilityDB{
			queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
				t.Fatal("DB query should not be called for owner")
				return nil
			},
		}
		err := AssertCapabilityTx(context.Background(), db, propID, userID, "owner", "expenses.post")
		if err != nil {
			t.Fatalf("expected nil for owner, got %v", err)
		}
	})

	t.Run("Tenant role returns ErrManagerInactive", func(t *testing.T) {
		db := &mockCapabilityDB{}
		err := AssertCapabilityTx(context.Background(), db, propID, userID, "tenant", "expenses.post")
		if !errors.Is(err, ErrManagerInactive) {
			t.Fatalf("expected ErrManagerInactive, got %v", err)
		}
	})

	t.Run("Manager user not found in DB returns ErrManagerInactive", func(t *testing.T) {
		db := &mockCapabilityDB{
			queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
				return &mockUserRow{err: pgx.ErrNoRows}
			},
		}
		err := AssertCapabilityTx(context.Background(), db, propID, userID, "manager", "expenses.post")
		if !errors.Is(err, ErrManagerInactive) {
			t.Fatalf("expected ErrManagerInactive, got %v", err)
		}
	})

	t.Run("Manager property mismatch returns ErrManagerInactive", func(t *testing.T) {
		db := &mockCapabilityDB{
			queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
				return &mockUserRow{role: "manager", propID: &otherPropID}
			},
		}
		err := AssertCapabilityTx(context.Background(), db, propID, userID, "manager", "expenses.post")
		if !errors.Is(err, ErrManagerInactive) {
			t.Fatalf("expected ErrManagerInactive, got %v", err)
		}
	})

	t.Run("Manager role mismatch in DB returns ErrManagerInactive", func(t *testing.T) {
		db := &mockCapabilityDB{
			queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
				return &mockUserRow{role: "tenant", propID: &propID}
			},
		}
		err := AssertCapabilityTx(context.Background(), db, propID, userID, "manager", "expenses.post")
		if !errors.Is(err, ErrManagerInactive) {
			t.Fatalf("expected ErrManagerInactive, got %v", err)
		}
	})

	t.Run("Disabled manager returns ErrManagerInactive", func(t *testing.T) {
		now := time.Now().UTC()
		db := &mockCapabilityDB{
			queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
				return &mockUserRow{role: "manager", propID: &propID, disabledAt: &now}
			},
		}
		err := AssertCapabilityTx(context.Background(), db, propID, userID, "manager", "expenses.post")
		if !errors.Is(err, ErrManagerInactive) {
			t.Fatalf("expected ErrManagerInactive for disabled manager, got %v", err)
		}
	})

	t.Run("Manager capability revoked or missing returns ErrCapabilityDenied", func(t *testing.T) {
		queryCount := 0
		db := &mockCapabilityDB{
			queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
				queryCount++
				if queryCount == 1 {
					return &mockUserRow{role: "manager", propID: &propID}
				}
				return &mockIntRow{err: pgx.ErrNoRows}
			},
		}
		err := AssertCapabilityTx(context.Background(), db, propID, userID, "manager", "expenses.post")
		if !errors.Is(err, ErrCapabilityDenied) {
			t.Fatalf("expected ErrCapabilityDenied, got %v", err)
		}
	})

	t.Run("Active manager with active grant succeeds", func(t *testing.T) {
		queryCount := 0
		db := &mockCapabilityDB{
			queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
				queryCount++
				if queryCount == 1 {
					return &mockUserRow{role: "manager", propID: &propID}
				}
				return &mockIntRow{val: 1}
			},
		}
		err := AssertCapabilityTx(context.Background(), db, propID, userID, "manager", "expenses.post")
		if err != nil {
			t.Fatalf("expected nil for active manager with capability, got %v", err)
		}
		if queryCount != 2 {
			t.Fatalf("expected 2 hierarchical queries, got %d", queryCount)
		}
	})
}

func TestCapabilityRepo_TxLockHierarchy(t *testing.T) {
	propID := uuid.New()
	userID := uuid.New()
	grantedBy := uuid.New()

	t.Run("GrantTx locks users FOR UPDATE before inserting capability", func(t *testing.T) {
		step := 0
		db := &mockCapabilityDB{
			queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
				step++
				if !strings.Contains(sql, "FROM users WHERE id = $1 FOR UPDATE") {
					t.Fatalf("step 1 must lock users FOR UPDATE, got sql: %s", sql)
				}
				return &mockUserRow{propID: &propID}
			},
			execFn: func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
				if step != 1 {
					t.Fatalf("exec must follow user lock step 1, got step: %d", step)
				}
				if !strings.Contains(sql, "INSERT INTO manager_capabilities") {
					t.Fatalf("exec must insert into manager_capabilities, got: %s", sql)
				}
				return pgconn.NewCommandTag("INSERT 0 1"), nil
			},
		}
		repo := NewCapabilityRepo(db)
		err := repo.GrantTx(context.Background(), db, propID, userID, "expenses.post", grantedBy)
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
		if step != 1 {
			t.Fatalf("expected user lock step executed, got %d", step)
		}
	})

	t.Run("RevokeTx locks users FOR UPDATE before revoking capability", func(t *testing.T) {
		step := 0
		db := &mockCapabilityDB{
			queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
				step++
				if !strings.Contains(sql, "FROM users WHERE id = $1 FOR UPDATE") {
					t.Fatalf("step 1 must lock users FOR UPDATE, got sql: %s", sql)
				}
				return &mockUserRow{propID: &propID}
			},
			execFn: func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
				if step != 1 {
					t.Fatalf("exec must follow user lock step 1, got step: %d", step)
				}
				if !strings.Contains(sql, "UPDATE manager_capabilities SET revoked_at = NOW()") {
					t.Fatalf("exec must update manager_capabilities, got: %s", sql)
				}
				return pgconn.NewCommandTag("UPDATE 1"), nil
			},
		}
		repo := NewCapabilityRepo(db)
		err := repo.RevokeTx(context.Background(), db, propID, userID, "expenses.post")
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
		if step != 1 {
			t.Fatalf("expected user lock step executed, got %d", step)
		}
	})

	t.Run("RevokeAllTx locks users FOR UPDATE before revoking all capabilities", func(t *testing.T) {
		step := 0
		db := &mockCapabilityDB{
			queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
				step++
				if !strings.Contains(sql, "FROM users WHERE id = $1 FOR UPDATE") {
					t.Fatalf("step 1 must lock users FOR UPDATE, got sql: %s", sql)
				}
				return &mockUserRow{propID: &propID}
			},
			execFn: func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
				if step != 1 {
					t.Fatalf("exec must follow user lock step 1, got step: %d", step)
				}
				if !strings.Contains(sql, "UPDATE manager_capabilities SET revoked_at = NOW()") {
					t.Fatalf("exec must update manager_capabilities, got: %s", sql)
				}
				return pgconn.NewCommandTag("UPDATE 3"), nil
			},
		}
		repo := NewCapabilityRepo(db)
		err := repo.RevokeAllTx(context.Background(), db, propID, userID)
		if err != nil {
			t.Fatalf("expected nil, got %v", err)
		}
		if step != 1 {
			t.Fatalf("expected user lock step executed, got %d", step)
		}
	})

	t.Run("Lock failure in RevokeAllTx fails fast without executing Level 2", func(t *testing.T) {
		execCalled := false
		db := &mockCapabilityDB{
			queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
				return &mockUserRow{err: pgx.ErrNoRows}
			},
			execFn: func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
				execCalled = true
				return pgconn.CommandTag{}, nil
			},
		}
		repo := NewCapabilityRepo(db)
		err := repo.RevokeAllTx(context.Background(), db, propID, userID)
		if err == nil {
			t.Fatalf("expected error on missing user, got nil")
		}
		if execCalled {
			t.Fatalf("Level 2 exec must not be called when Level 1 lock fails")
		}
	})
}
