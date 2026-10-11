package gamification

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestErrorsIsDuplicate(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "real SQLSTATE 23505",
			err:      &pgconn.PgError{Code: "23505"},
			expected: true,
		},
		{
			name:     "wrapped SQLSTATE 23505",
			err:      fmt.Errorf("db error: %w", &pgconn.PgError{Code: "23505"}),
			expected: true,
		},
		{
			name:     "different SQLSTATE 23503",
			err:      &pgconn.PgError{Code: "23503"},
			expected: false,
		},
		{
			name:     "message containing duplicate key with no SQLSTATE",
			err:      errors.New("duplicate key value violates unique constraint"),
			expected: false,
		},
		{
			name:     "arbitrary error",
			err:      errors.New("connection reset by peer"),
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := errorsIsDuplicate(tc.err)
			if got != tc.expected {
				t.Fatalf("errorsIsDuplicate(%v) = %v, want %v", tc.err, got, tc.expected)
			}
		})
	}
}
