package postgres

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

// setupLiveTestPool initializes a database pool for integration tests using ONLY
// an explicit TEST_DATABASE_URL (never loading application .env or DATABASE_URL).
// It verifies the target is a disposable test database on PostgreSQL >= 17 before
// returning the pool and closes it via t.Cleanup.
func setupLiveTestPool(t *testing.T, timeout time.Duration) (*pgxpool.Pool, *config.Config) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping live Postgres test in -short mode")
		return nil, nil
	}
	testDBURL := testutil.RequireDB(t)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	pool, err := NewPool(ctx, testDBURL)
	if err != nil {
		testutil.FailOnSkipfIfDBRequired(t, "cannot connect to Postgres (%v), skipping live test", err)
		return nil, nil
	}
	t.Cleanup(func() {
		pool.Close()
	})

	var dbName string
	var serverVersionNum int
	if err := pool.QueryRow(ctx, `SELECT current_database(), current_setting('server_version_num')::int`).Scan(&dbName, &serverVersionNum); err != nil {
		t.Fatalf("live test safety check failed: %v", err)
		return nil, nil
	}
	if !strings.Contains(strings.ToLower(dbName), "test") {
		t.Fatalf("refusing to run live test against non-test database %q; TEST_DATABASE_URL must target a disposable test database", dbName)
		return nil, nil
	}
	if serverVersionNum < 170000 {
		t.Fatalf("live test requires PostgreSQL >= 17 (server_version_num >= 170000), got %d on database %q", serverVersionNum, dbName)
		return nil, nil
	}

	return pool, &config.Config{DatabaseURL: testDBURL}
}
