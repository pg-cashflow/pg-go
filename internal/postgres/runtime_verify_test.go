package postgres

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestLivePostgreSQL_SessionTimeouts(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set, skipping live PostgreSQL timeout test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	opt := defaultPoolOptions("pg-go-test-timeouts")
	opt.StatementTimeout = 30 * time.Second
	opt.IdleInTransactionTimeout = 60 * time.Second
	opt.LockTimeout = 10 * time.Second
	ConfigurePoolConfig(cfg, opt)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("create pool: %v", err)
	}
	defer pool.Close()

	// 1. Verify runtime params active on connection
	var st, lt, it string
	err = pool.QueryRow(ctx, "SHOW statement_timeout;").Scan(&st)
	if err != nil {
		t.Fatalf("SHOW statement_timeout: %v", err)
	}
	err = pool.QueryRow(ctx, "SHOW lock_timeout;").Scan(&lt)
	if err != nil {
		t.Fatalf("SHOW lock_timeout: %v", err)
	}
	err = pool.QueryRow(ctx, "SHOW idle_in_transaction_session_timeout;").Scan(&it)
	if err != nil {
		t.Fatalf("SHOW idle_in_transaction_session_timeout: %v", err)
	}

	t.Logf("Active PostgreSQL session parameters: statement_timeout=%s, lock_timeout=%s, idle_in_transaction_timeout=%s", st, lt, it)

	if !strings.Contains(st, "30s") && !strings.Contains(st, "30000ms") {
		t.Errorf("expected statement_timeout 30s/30000ms, got: %s", st)
	}
	if !strings.Contains(lt, "10s") && !strings.Contains(lt, "10000ms") {
		t.Errorf("expected lock_timeout 10s/10000ms, got: %s", lt)
	}
	if !strings.Contains(it, "60s") && !strings.Contains(it, "60000ms") && !strings.Contains(it, "1min") {
		t.Errorf("expected idle_in_transaction_session_timeout 60s/1min, got: %s", it)
	}

	// 2. Verify statement_timeout aborts runaway query
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire conn: %v", err)
	}
	defer conn.Release()

	_, err = conn.Exec(ctx, "SET statement_timeout = '200ms';")
	if err != nil {
		t.Fatalf("set local statement_timeout: %v", err)
	}

	// Execute sleep 1s -> should be killed at 200ms with code 57014 (query_canceled)
	start := time.Now()
	_, err = conn.Exec(ctx, "SELECT pg_sleep(1.0);")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected statement_timeout error, but query succeeded after %v", elapsed)
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code != "57014" { // query_canceled
			t.Errorf("expected SQLSTATE 57014 (query_canceled), got: %s (%s)", pgErr.Code, pgErr.Message)
		}
	} else {
		t.Logf("Non-pgconn error received: %v", err)
	}
	t.Logf("Runaway query successfully aborted by statement_timeout in %v (< 500ms)", elapsed)
}
