package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func defaultPoolOptions(appName string) PoolOptions {
	maxConns := 25
	if s := os.Getenv("DATABASE_MAX_CONNS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			maxConns = n
		}
	}
	minConns := maxConns / 5
	if minConns < 2 {
		minConns = 2
	}
	if s := os.Getenv("DATABASE_MIN_CONNS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			minConns = n
		}
	}
	maxLifetime := time.Hour
	if s := os.Getenv("DATABASE_MAX_CONN_LIFETIME"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			maxLifetime = d
		}
	}
	maxIdle := 15 * time.Minute
	if s := os.Getenv("DATABASE_MAX_CONN_IDLE_TIME"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			maxIdle = d
		}
	}
	connectTimeout := 10 * time.Second
	if s := os.Getenv("DATABASE_CONNECT_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			connectTimeout = d
		}
	}
	tcpKeepAlive := 30 * time.Second
	if s := os.Getenv("DATABASE_TCP_KEEPALIVE"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			tcpKeepAlive = d
		}
	}
	pgbouncer := false
	if s := strings.ToLower(os.Getenv("DATABASE_PGBOUNCER")); s == "1" || s == "true" || s == "yes" {
		pgbouncer = true
	}

	slowQueryThreshold := 100 * time.Millisecond
	if s := os.Getenv("DATABASE_SLOW_QUERY_THRESHOLD"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			slowQueryThreshold = d
		}
	}

	statementTimeout := 30 * time.Second
	if s := os.Getenv("DATABASE_STATEMENT_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			statementTimeout = d
		}
	}
	idleInTxTimeout := 60 * time.Second
	if s := os.Getenv("DATABASE_IDLE_IN_TRANSACTION_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			idleInTxTimeout = d
		}
	}
	lockTimeout := 10 * time.Second
	if s := os.Getenv("DATABASE_LOCK_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			lockTimeout = d
		}
	}

	return PoolOptions{
		MaxConns:                 maxConns,
		MinConns:                 minConns,
		MaxConnLifetime:          maxLifetime,
		MaxConnIdleTime:          maxIdle,
		HealthCheckPeriod:        1 * time.Minute,
		ConnectTimeout:           connectTimeout,
		TCPKeepAlive:             tcpKeepAlive,
		PgBouncer:                pgbouncer,
		AppName:                  appName,
		StatementTimeout:         statementTimeout,
		IdleInTransactionTimeout: idleInTxTimeout,
		LockTimeout:              lockTimeout,
		Tracer:                   NewQueryPerfTracer(slowQueryThreshold, nil),
	}
}

func NewPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}

	opt := defaultPoolOptions("pg-go-main")
	// If URL points to standard PgBouncer port 6432, auto-enable PgBouncer mode
	if cfg.ConnConfig.Port == 6432 {
		opt.PgBouncer = true
	}
	ConfigurePoolConfig(cfg, opt)

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

// NewSearchPool creates a dedicated small connection pool (default 10 connections; per-request fan-out is capped at 4) specifically
// for Search V2 queries, isolating search traffic from money paths (webhooks, ledger, payouts).
func NewSearchPool(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database url for search pool: %w", err)
	}

	opt := defaultPoolOptions("pg-go-search")
	maxConns := 10
	if s := os.Getenv("SEARCH_DATABASE_MAX_CONNS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			maxConns = n
		}
	}
	opt.MaxConns = maxConns
	minConns := 2
	if s := os.Getenv("SEARCH_DATABASE_MIN_CONNS"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			minConns = n
		}
	}
	opt.MinConns = minConns
	opt.MaxConnLifetime = 30 * time.Minute
	opt.MaxConnIdleTime = 5 * time.Minute

	if cfg.ConnConfig.Port == 6432 {
		opt.PgBouncer = true
	}
	ConfigurePoolConfig(cfg, opt)

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect search pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping search pool: %w", err)
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
	// checksum records the SHA-256 of each migration file as it was applied, so an edit to an
	// already-applied migration is detected instead of being silently ignored.
	if _, err := conn.Exec(ctx, `ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum TEXT`); err != nil {
		return fmt.Errorf("add schema_migrations.checksum: %w", err)
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
		body, err := os.ReadFile(filepath.Join(dir, filepath.Clean(name))) // #nosec G304
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

		sum := sha256.Sum256(body)
		checksum := hex.EncodeToString(sum[:])

		// Check existence inside the locked transaction to prevent TOCTOU races between concurrent runners
		var appliedChecksum *string
		err = tx.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=$1`, name).Scan(&appliedChecksum)
		switch {
		case err == nil:
			// Already applied. Verify the file has not been edited since.
			if appliedChecksum == nil {
				// Applied before checksums existed: record the current content (trust on first use).
				if _, err := tx.Exec(ctx, `UPDATE schema_migrations SET checksum=$2 WHERE version=$1 AND checksum IS NULL`, name, checksum); err != nil {
					_ = tx.Rollback(ctx)
					return fmt.Errorf("record checksum for %s: %w", name, err)
				}
				if err := tx.Commit(ctx); err != nil {
					return err
				}
				continue
			}
			if *appliedChecksum != checksum {
				_ = tx.Rollback(ctx)
				return fmt.Errorf("migration %s was modified after it was applied (applied checksum %s, file checksum %s): never edit an applied migration, add a new one", name, *appliedChecksum, checksum)
			}
			_ = tx.Rollback(ctx)
			continue
		case errors.Is(err, pgx.ErrNoRows):
			// Not applied yet: fall through and apply it.
		default:
			_ = tx.Rollback(ctx)
			return err
		}

		if _, err := tx.Exec(ctx, string(body)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("apply %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version, checksum) VALUES($1, $2)`, name, checksum); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}
