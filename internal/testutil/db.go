package testutil

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/config"
)

var defaultTestDBAllowlist = map[string]struct{}{
	"pg_test":          {},
	"pg_cashflow_test": {},
}

// ValidateLiveTestDB verifies that dbName is an exact match for an allowlisted
// disposable test database and that the PostgreSQL server is version 17 or newer.
func ValidateLiveTestDB(dbName string, serverVersionNum int, allowlistEnv string, extraAllowlist ...string) error {
	allowed := make(map[string]struct{}, len(defaultTestDBAllowlist)+len(extraAllowlist))
	for k := range defaultTestDBAllowlist {
		allowed[k] = struct{}{}
	}
	for _, entry := range strings.Split(allowlistEnv, ",") {
		trimmed := strings.TrimSpace(strings.ToLower(entry))
		if trimmed != "" {
			allowed[trimmed] = struct{}{}
		}
	}
	for _, entry := range extraAllowlist {
		trimmed := strings.TrimSpace(strings.ToLower(entry))
		if trimmed != "" {
			allowed[trimmed] = struct{}{}
		}
	}

	normDB := strings.TrimSpace(strings.ToLower(dbName))
	if _, ok := allowed[normDB]; !ok {
		return fmt.Errorf("refusing to run live test against non-allowlisted database %q; TEST_DATABASE_URL must target pg_test, pg_cashflow_test, or a database in TEST_DATABASE_ALLOWLIST", dbName)
	}
	if serverVersionNum < 170000 {
		return fmt.Errorf("live test requires PostgreSQL >= 17 (server_version_num >= 170000), got %d on database %q", serverVersionNum, dbName)
	}
	return nil
}

// ValidateLiveTestPool queries the connected database identity and server version
// and enforces ValidateLiveTestDB.
func ValidateLiveTestPool(ctx context.Context, pool *pgxpool.Pool, extraAllowlist ...string) error {
	var dbName string
	var serverVersionNum int
	if err := pool.QueryRow(ctx, `SELECT current_database(), current_setting('server_version_num')::int`).Scan(&dbName, &serverVersionNum); err != nil {
		return fmt.Errorf("live test safety check failed: %w", err)
	}
	return ValidateLiveTestDB(dbName, serverVersionNum, os.Getenv("TEST_DATABASE_ALLOWLIST"), extraAllowlist...)
}

// ValidateLiveTestURL connects to dbURL briefly and verifies it against
// ValidateLiveTestPool.
func ValidateLiveTestURL(ctx context.Context, dbURL string, extraAllowlist ...string) error {
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	return ValidateLiveTestPool(ctx, pool, extraAllowlist...)
}

// ValidateLiveTestServerVersion verifies that the target server is PostgreSQL >= 17
// without requiring the database name to be in the test DB allowlist (used for
// administrative /postgres connections that create/drop disposable test DBs).
func ValidateLiveTestServerVersion(ctx context.Context, dbURL string) error {
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	var dbName string
	var serverVersionNum int
	if err := pool.QueryRow(ctx, `SELECT current_database(), current_setting('server_version_num')::int`).Scan(&dbName, &serverVersionNum); err != nil {
		return fmt.Errorf("live test server version check failed: %w", err)
	}
	if serverVersionNum < 170000 {
		return fmt.Errorf("live test requires PostgreSQL >= 17 (server_version_num >= 170000), got %d on database %q", serverVersionNum, dbName)
	}
	return nil
}

// RequireDB ensures that if REQUIRE_DB=1 is set, the test fails immediately
// via t.Fatalf if TEST_DATABASE_URL is missing. If REQUIRE_DB is not set, it calls t.Skip.
// Never falls back to DATABASE_URL so live tests cannot accidentally target a non-test DB.
func RequireDB(t testing.TB) string {
	t.Helper()
	dbURL := os.Getenv("TEST_DATABASE_URL")
	if dbURL == "" {
		if os.Getenv("REQUIRE_DB") == "1" {
			t.Fatalf("REQUIRE_DB=1 but TEST_DATABASE_URL is missing or empty")
			return ""
		}
		t.Skip("TEST_DATABASE_URL not set, skipping live database test")
		return ""
	}
	return dbURL
}

// RequireValidatedDB requires TEST_DATABASE_URL, verifies connectivity, and enforces
// the exact-name allowlist + PostgreSQL >= 17 check before returning the validated URL.
func RequireValidatedDB(t testing.TB, timeout time.Duration, extraAllowlist ...string) string {
	t.Helper()
	dbURL := RequireDB(t)
	if dbURL == "" {
		return ""
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		FailOnSkipfIfDBRequired(t, "cannot connect to Postgres (%v), skipping live test", err)
		return ""
	}
	defer pool.Close()

	var dbName string
	var serverVersionNum int
	if err := pool.QueryRow(ctx, `SELECT current_database(), current_setting('server_version_num')::int`).Scan(&dbName, &serverVersionNum); err != nil {
		FailOnSkipfIfDBRequired(t, "cannot query Postgres safety metadata (%v), skipping live test", err)
		return ""
	}
	if err := ValidateLiveTestDB(dbName, serverVersionNum, os.Getenv("TEST_DATABASE_ALLOWLIST"), extraAllowlist...); err != nil {
		t.Fatal(err)
		return ""
	}
	return dbURL
}

// RequireValidatedSecondaryURL reads a secondary test database URL env var (such as
// TEST_DATABASE_MAINT_URL or TEST_DATABASE_WORKER_URL). If unset, it returns fallbackURL.
// If set, it validates the database against the allowlist and PostgreSQL >= 17 check.
// Never reads non-TEST_* database env vars.
func RequireValidatedSecondaryURL(t testing.TB, envVar, fallbackURL string, timeout time.Duration, extraAllowlist ...string) string {
	t.Helper()
	val := strings.TrimSpace(os.Getenv(envVar))
	if val == "" {
		return fallbackURL
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := ValidateLiveTestURL(ctx, val, extraAllowlist...); err != nil {
		t.Fatalf("%s failed live test safety check: %v", envVar, err)
		return ""
	}
	return val
}

// LoadTestConfig loads application config for live tests using ONLY validated
// TEST_DATABASE_* credentials (never reading DATABASE_URL, DATABASE_MAINT_URL, or
// DATABASE_WORKER_URL from the shell or .env). Safe test-only defaults are injected
// for required non-DB secrets if unset in the process environment.
func LoadTestConfig(t testing.TB, validatedDBURL string, timeout time.Duration) *config.Config {
	t.Helper()
	maintURL := RequireValidatedSecondaryURL(t, "TEST_DATABASE_MAINT_URL", validatedDBURL, timeout)
	workerURL := RequireValidatedSecondaryURL(t, "TEST_DATABASE_WORKER_URL", maintURL, timeout)

	t.Setenv("DATABASE_URL", validatedDBURL)
	t.Setenv("DATABASE_MAINT_URL", maintURL)
	t.Setenv("DATABASE_WORKER_URL", workerURL)

	if os.Getenv("APP_ENV") == "" {
		t.Setenv("APP_ENV", "test")
	}
	if os.Getenv("JWT_SECRET") == "" {
		t.Setenv("JWT_SECRET", "test-jwt-secret-do-not-use-in-production-32chars")
	}
	if os.Getenv("OTP_HMAC_SECRET") == "" {
		t.Setenv("OTP_HMAC_SECRET", "test-otp-hmac-secret-do-not-use-in-prod-32chars")
	}
	if os.Getenv("MAGIC_LINK_HMAC_SECRET") == "" {
		t.Setenv("MAGIC_LINK_HMAC_SECRET", "test-magic-link-hmac-secret-not-for-prod-32chars")
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("load test config: %v", err)
		return nil
	}
	cfg.DatabaseURL = validatedDBURL
	cfg.DatabaseMaintURL = maintURL
	cfg.DatabaseWorkerURL = workerURL
	return cfg
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
