package postgres

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	maxConns := 25
	if s := os.Getenv("DATABASE_MAX_CONNS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			maxConns = n
		}
	}
	cfg.MaxConns = int32(maxConns)
	minConns := maxConns / 5
	if minConns < 2 {
		minConns = 2
	}
	if s := os.Getenv("DATABASE_MIN_CONNS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			minConns = n
		}
	}
	cfg.MinConns = int32(minConns)
	cfg.MaxConnLifetime = time.Hour
	if s := os.Getenv("DATABASE_MAX_CONN_LIFETIME"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			cfg.MaxConnLifetime = d
		}
	}
	cfg.MaxConnIdleTime = 15 * time.Minute
	if s := os.Getenv("DATABASE_MAX_CONN_IDLE_TIME"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			cfg.MaxConnIdleTime = d
		}
	}
	cfg.HealthCheckPeriod = 1 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// Migrate applies pending *.sql files from dir in lexical order.
// Serializes concurrent migrations across processes or concurrent test packages
// using transaction-scoped advisory locks (0x50474D4947524154 / 'PGMIGRAT'),
// ensuring full compatibility with both direct connections and PgBouncer transaction-mode pooling.
func Migrate(ctx context.Context, pool *pgxpool.Pool, dir string) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquire conn for migration: %w", err)
	}
	defer conn.Release()

	// 64-bit advisory lock key allocated to pg-go schema migrations: ASCII 'PGMIGRAT' (0x50474D4947524154).
	const migrationLockID int64 = 0x50474D4947524154 // 'PGMIGRAT'

	_, err = conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`)
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)

	for _, name := range files {
		body, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return err
		}

		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}

		// Transaction-level advisory lock guarantees serialized execution across concurrent workers
		// even when connected through PgBouncer in transaction-pooling mode.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, migrationLockID); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("acquire migration tx advisory lock for %s: %w", name, err)
		}

		// Check existence inside the locked transaction to prevent TOCTOU races between concurrent runners
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, name).Scan(&exists); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if exists {
			_ = tx.Rollback(ctx)
			continue
		}

		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
