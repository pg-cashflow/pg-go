package gamification

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestErrorsIsDuplicate_UsesSQLState(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "real SQLSTATE 23505",
			err:      &pgconn.PgError{Code: "23505"},
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
			name:     "nil error",
			err:      nil,
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
