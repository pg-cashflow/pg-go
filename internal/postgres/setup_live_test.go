package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/testutil"
)

// setupLiveTestPool initializes a database pool for integration tests with FailOnSkip enforcement.
// It automatically closes the pool when the test completes via t.Cleanup.
func setupLiveTestPool(t *testing.T, timeout time.Duration) (*pgxpool.Pool, *config.Config) {
	t.Helper()
	_ = godotenv.Load("../../.env")
	testutil.RequireDB(t)

	cfg, err := config.Load()
	if err != nil {
		testutil.FailOnSkipIfDBRequired(t, "config load failed, skipping live Postgres test")
		return nil, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	pool, err := NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		testutil.FailOnSkipfIfDBRequired(t, "cannot connect to Postgres (%v), skipping live test", err)
		return nil, nil
	}

	t.Cleanup(func() {
		pool.Close()
	})

	return pool, cfg
}
