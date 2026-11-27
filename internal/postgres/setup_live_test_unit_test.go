package postgres

import (
	"testing"

	"github.com/pg-cashflow/pg-go/internal/testutil"
)

func TestValidateLiveTestDB_AllowlistAndVersion(t *testing.T) {
	// Default allowlisted DBs on PostgreSQL 17+ succeed.
	if err := testutil.ValidateLiveTestDB("pg_test", 170002, ""); err != nil {
		t.Fatalf("expected pg_test on PG 17 to pass, got %v", err)
	}
	if err := testutil.ValidateLiveTestDB("pg_cashflow_test", 170000, ""); err != nil {
		t.Fatalf("expected pg_cashflow_test on PG 17 to pass, got %v", err)
	}

	// Substring "test" alone is rejected if not in allowlist.
	if err := testutil.ValidateLiveTestDB("prod_test_replica", 170002, ""); err == nil {
		t.Fatal("expected non-allowlisted database containing 'test' to be rejected")
	}

	// Custom CI allowlist entry succeeds.
	if err := testutil.ValidateLiveTestDB("ci_ephemeral_test_42", 170002, "ci_ephemeral_test_42, other_test"); err != nil {
		t.Fatalf("expected allowlisted CI database to pass, got %v", err)
	}

	// PostgreSQL < 17 is rejected even on an allowlisted DB.
	if err := testutil.ValidateLiveTestDB("pg_test", 160004, ""); err == nil {
		t.Fatal("expected PostgreSQL 16 to be rejected")
	}
}
