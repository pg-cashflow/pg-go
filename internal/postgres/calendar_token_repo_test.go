package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type mockCalendarTokenRow struct {
	revokedAt *time.Time
	err       error
}

func (r *mockCalendarTokenRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if len(dest) > 0 {
		if ptr, ok := dest[0].(**time.Time); ok {
			*ptr = r.revokedAt
			return nil
		}
	}
	return nil
}

type mockCalendarDB struct {
	queryRowFn func(ctx context.Context, sql string, args ...any) pgx.Row
	execFn     func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func (m *mockCalendarDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if m.execFn != nil {
		return m.execFn(ctx, sql, args...)
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (m *mockCalendarDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return nil, nil
}

func (m *mockCalendarDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if m.queryRowFn != nil {
		return m.queryRowFn(ctx, sql, args...)
	}
	return &mockCalendarTokenRow{}
}

func TestCalendarTokenRepo_InsertToken(t *testing.T) {
	propID := uuid.New()
	tokenHash := "abc123hash"
	expiresAt := time.Now().Add(24 * time.Hour)
	dbErr := errors.New("db error")

	tests := []struct {
		name    string
		execErr error
		wantErr bool
	}{
		{
			name:    "Successful insert",
			execErr: nil,
			wantErr: false,
		},
		{
			name:    "Database execution failure",
			execErr: dbErr,
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &mockCalendarDB{
				execFn: func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
					return pgconn.NewCommandTag("INSERT 1"), tc.execErr
				},
			}
			repo := NewCalendarTokenRepo(db)
			err := repo.InsertToken(context.Background(), propID, tokenHash, expiresAt)

			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestCalendarTokenRepo_GetTokenRevocation(t *testing.T) {
	propID := uuid.New()
	tokenHash := "abc123hash"
	now := time.Now()
	dbErr := errors.New("query failed")

	tests := []struct {
		name        string
		row         *mockCalendarTokenRow
		wantRevoked bool
		wantExists  bool
		wantErr     bool
	}{
		{
			name:        "Active token exists",
			row:         &mockCalendarTokenRow{revokedAt: nil},
			wantRevoked: false,
			wantExists:  true,
			wantErr:     false,
		},
		{
			name:        "Revoked token exists",
			row:         &mockCalendarTokenRow{revokedAt: &now},
			wantRevoked: true,
			wantExists:  true,
			wantErr:     false,
		},
		{
			name:        "Token not found",
			row:         &mockCalendarTokenRow{err: pgx.ErrNoRows},
			wantRevoked: false,
			wantExists:  false,
			wantErr:     false,
		},
		{
			name:        "Database error",
			row:         &mockCalendarTokenRow{err: dbErr},
			wantRevoked: false,
			wantExists:  false,
			wantErr:     true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &mockCalendarDB{
				queryRowFn: func(ctx context.Context, sql string, args ...any) pgx.Row {
					return tc.row
				},
			}
			repo := NewCalendarTokenRepo(db)
			revoked, exists, err := repo.GetTokenRevocation(context.Background(), propID, tokenHash)

			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if revoked != tc.wantRevoked {
				t.Fatalf("expected revoked=%v, got %v", tc.wantRevoked, revoked)
			}
			if exists != tc.wantExists {
				t.Fatalf("expected exists=%v, got %v", tc.wantExists, exists)
			}
		})
	}
}

func TestCalendarTokenRepo_RevokeTokens(t *testing.T) {
	propID := uuid.New()
	dbErr := errors.New("db error")

	tests := []struct {
		name      string
		rowsCount int64
		execErr   error
		wantCount int64
		wantErr   bool
	}{
		{
			name:      "Revoke multiple tokens",
			rowsCount: 3,
			execErr:   nil,
			wantCount: 3,
			wantErr:   false,
		},
		{
			name:      "Database failure",
			rowsCount: 0,
			execErr:   dbErr,
			wantCount: 0,
			wantErr:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := &mockCalendarDB{
				execFn: func(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
					return pgconn.NewCommandTag("UPDATE 3"), tc.execErr
				},
			}
			repo := NewCalendarTokenRepo(db)
			count, err := repo.RevokeTokens(context.Background(), propID)

			if tc.wantErr && err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.wantErr && count != tc.wantCount {
				t.Fatalf("expected count=%d, got %d", tc.wantCount, count)
			}
		})
	}
}
