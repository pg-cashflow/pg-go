package testutil

import (
	"fmt"
	"os"
	"testing"
)

// RequireDB ensures that if REQUIRE_DB=1 is set, the test fails immediately
// via t.Fatalf if DATABASE_URL is missing. If REQUIRE_DB is not set, it calls t.Skip.
// Returns the valid database URL.
func RequireDB(t testing.TB) string {
	t.Helper()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		if os.Getenv("REQUIRE_DB") == "1" {
			t.Fatalf("REQUIRE_DB=1 but DATABASE_URL is missing or empty")
			return ""
		}
		t.Skip("DATABASE_URL not set, skipping live database test")
		return ""
	}
	return dbURL
}

// FailOnSkipIfDBRequired checks if REQUIRE_DB=1. If set, it fails with t.Fatalf
// instead of silently skipping. Otherwise it calls t.Skip with the given reason.
func FailOnSkipIfDBRequired(t testing.TB, reason string) {
	t.Helper()
	if os.Getenv("REQUIRE_DB") == "1" {
		t.Fatalf("REQUIRE_DB=1 but test was about to skip: %s", reason)
		return
	}
	t.Skip(reason)
}

// FailOnSkipfIfDBRequired formats the reason and checks if REQUIRE_DB=1.
// If set, it fails with t.Fatalf instead of silently skipping.
func FailOnSkipfIfDBRequired(t testing.TB, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv("REQUIRE_DB") == "1" {
		t.Fatalf("REQUIRE_DB=1 but test was about to skip: %s", msg)
		return
	}
	t.Skip(msg)
}
