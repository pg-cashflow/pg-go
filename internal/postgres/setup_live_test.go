package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

// setupLiveTestPool initializes a database pool for integration tests using ONLY
// an explicit TEST_DATABASE_URL (never loading application .env or DATABASE_URL).
// It verifies the target is an allowlisted disposable test database on PostgreSQL >= 17
// before returning the pool and closes it via t.Cleanup.
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

	if err := testutil.ValidateLiveTestPool(ctx, pool); err != nil {
		t.Fatal(err)
		return nil, nil
	}

	return pool, &config.Config{DatabaseURL: testDBURL}
}
