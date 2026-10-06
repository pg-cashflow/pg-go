package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

// ScopedDB wraps *pgxpool.Pool and sets app.current_property_id before executing queries.
// It pipelines the session scope with the query in a single pgx.Batch round trip and enforces
// transaction-local SET LOCAL configuration so it is safe under PgBouncer transaction pooling.
type ScopedDB struct {
	pool *pgxpool.Pool
}

// NewScopedDB constructs a property-scoped DB handle wrapping a pool.
func NewScopedDB(pool *pgxpool.Pool) *ScopedDB {
	return &ScopedDB{pool: pool}
}

// Exec executes a statement. If property scope is active, it runs within an atomic
// pipelined transaction block with transaction-local scope (safe under PgBouncer).
func (s *ScopedDB) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	if tx, ok := TxFromContext(ctx); ok {
		return tx.Exec(ctx, sql, arguments...)
	}
	propID, hasProp := requestscope.PropertyIDFromContext(ctx)
	if !hasProp || propID == uuid.Nil {
		return s.pool.Exec(ctx, sql, arguments...)
	}

	batch := &pgx.Batch{}
	batch.Queue("BEGIN")
	batch.Queue("SELECT set_config('app.current_property_id', $1, true)", propID.String())
	batch.Queue(sql, arguments...)
	batch.Queue("COMMIT")

	br := s.pool.SendBatch(ctx, batch)
	defer br.Close()

	if _, err := br.Exec(); err != nil {
		return pgconn.CommandTag{}, fmt.Errorf("scoped exec begin: %w", err)
	}
	if _, err := br.Exec(); err != nil {
		return pgconn.CommandTag{}, fmt.Errorf("scoped exec set_config: %w", err)
	}
	tag, err := br.Exec()
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	if _, err := br.Exec(); err != nil {
		return pgconn.CommandTag{}, fmt.Errorf("scoped exec commit: %w", err)
	}
	return tag, nil
}

// Query executes a query returning rows. If property scope is active, it pipelines
// BEGIN + set_config + query + COMMIT in a single batch, releasing the connection and transaction
// when rows are closed.
func (s *ScopedDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if tx, ok := TxFromContext(ctx); ok {
		return tx.Query(ctx, sql, args...)
	}
	propID, hasProp := requestscope.PropertyIDFromContext(ctx)
	if !hasProp || propID == uuid.Nil {
		return s.pool.Query(ctx, sql, args...)
	}

	batch := &pgx.Batch{}
	batch.Queue("BEGIN")
	batch.Queue("SELECT set_config('app.current_property_id', $1, true)", propID.String())
	batch.Queue(sql, args...)
	batch.Queue("COMMIT")

	br := s.pool.SendBatch(ctx, batch)
	if _, err := br.Exec(); err != nil {
		_ = br.Close()
		return nil, fmt.Errorf("scoped query begin: %w", err)
	}
	if _, err := br.Exec(); err != nil {
		_ = br.Close()
		return nil, fmt.Errorf("scoped query set_config: %w", err)
	}
	rows, err := br.Query()
	if err != nil {
		_ = br.Close()
		return nil, err
	}
	return &batchedScopedRows{rows: rows, br: br}, nil
}

type batchedScopedRows struct {
	rows   pgx.Rows
	br     pgx.BatchResults
	closed bool
}

func (r *batchedScopedRows) Close() {
	if r.closed {
		return
	}
	r.closed = true
	r.rows.Close()
	if r.br != nil {
		_, _ = r.br.Exec() // Consume COMMIT
		_ = r.br.Close()
	}
}

func (r *batchedScopedRows) Err() error {
	return r.rows.Err()
}

func (r *batchedScopedRows) CommandTag() pgconn.CommandTag {
	return r.rows.CommandTag()
}

func (r *batchedScopedRows) FieldDescriptions() []pgconn.FieldDescription {
	return r.rows.FieldDescriptions()
}

func (r *batchedScopedRows) Next() bool {
	return r.rows.Next()
}

func (r *batchedScopedRows) Scan(dest ...any) error {
	return r.rows.Scan(dest...)
}

func (r *batchedScopedRows) Values() ([]any, error) {
	return r.rows.Values()
}

func (r *batchedScopedRows) RawValues() [][]byte {
	return r.rows.RawValues()
}

func (r *batchedScopedRows) Conn() *pgx.Conn {
	return r.rows.Conn()
}

// QueryRow executes a query expected to return at most one row.
// When scoped, it pipelines BEGIN + set_config + query + COMMIT in a single round trip.
func (s *ScopedDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if tx, ok := TxFromContext(ctx); ok {
		return tx.QueryRow(ctx, sql, args...)
	}
	propID, hasProp := requestscope.PropertyIDFromContext(ctx)
	if !hasProp || propID == uuid.Nil {
		return s.pool.QueryRow(ctx, sql, args...)
	}

	batch := &pgx.Batch{}
	batch.Queue("BEGIN")
	batch.Queue("SELECT set_config('app.current_property_id', $1, true)", propID.String())
	batch.Queue(sql, args...)
	batch.Queue("COMMIT")

	br := s.pool.SendBatch(ctx, batch)
	return &batchedScopedRow{br: br}
}

type batchedScopedRow struct {
	br pgx.BatchResults
}

func (r *batchedScopedRow) Scan(dest ...any) error {
	defer r.br.Close()
	if _, err := r.br.Exec(); err != nil {
		return fmt.Errorf("scoped query begin: %w", err)
	}
	if _, err := r.br.Exec(); err != nil {
		return fmt.Errorf("scoped query set_config: %w", err)
	}
	row := r.br.QueryRow()
	scanErr := row.Scan(dest...)
	if _, err := r.br.Exec(); err != nil && scanErr == nil {
		return fmt.Errorf("scoped query commit: %w", err)
	}
	return scanErr
}
