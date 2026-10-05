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
// If the property cannot be reset afterwards, the underlying connection is destroyed
// to prevent scope leakage between pooled requests.
type ScopedDB struct {
	pool *pgxpool.Pool
}

// NewScopedDB constructs a property-scoped DB handle wrapping a pool.
func NewScopedDB(pool *pgxpool.Pool) *ScopedDB {
	return &ScopedDB{pool: pool}
}

func (s *ScopedDB) Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error) {
	if tx, ok := TxFromContext(ctx); ok {
		return tx.Exec(ctx, sql, arguments...)
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer conn.Release()

	propID, hasProp := requestscope.PropertyIDFromContext(ctx)
	if hasProp && propID != uuid.Nil {
		if _, err := conn.Exec(ctx, "SELECT set_config('app.current_property_id', $1, false)", propID.String()); err != nil {
			conn.Conn().Close(ctx)
			return pgconn.CommandTag{}, fmt.Errorf("set app.current_property_id: %w", err)
		}
		defer func() {
			if _, err := conn.Exec(ctx, "RESET app.current_property_id"); err != nil {
				conn.Conn().Close(ctx)
			}
		}()
	}

	return conn.Exec(ctx, sql, arguments...)
}

func (s *ScopedDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if tx, ok := TxFromContext(ctx); ok {
		return tx.Query(ctx, sql, args...)
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	// For queries with rows, we use a short-lived transaction so SET LOCAL guarantees cleanup
	tx, err := conn.Begin(ctx)
	if err != nil {
		conn.Release()
		return nil, err
	}
	propID, hasProp := requestscope.PropertyIDFromContext(ctx)
	if hasProp && propID != uuid.Nil {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propID.String()); err != nil {
			_ = tx.Rollback(ctx)
			conn.Release()
			return nil, fmt.Errorf("set local app.current_property_id: %w", err)
		}
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		_ = tx.Rollback(ctx)
		conn.Release()
		return nil, err
	}
	return &scopedRows{rows: rows, tx: tx, conn: conn}, nil
}

type scopedRows struct {
	rows pgx.Rows
	tx   pgx.Tx
	conn *pgxpool.Conn
}

func (r *scopedRows) Close() {
	r.rows.Close()
	_ = r.tx.Rollback(context.Background())
	r.conn.Release()
}

func (r *scopedRows) Err() error {
	return r.rows.Err()
}

func (r *scopedRows) CommandTag() pgconn.CommandTag {
	return r.rows.CommandTag()
}

func (r *scopedRows) FieldDescriptions() []pgconn.FieldDescription {
	return r.rows.FieldDescriptions()
}

func (r *scopedRows) Next() bool {
	return r.rows.Next()
}

func (r *scopedRows) Scan(dest ...any) error {
	return r.rows.Scan(dest...)
}

func (r *scopedRows) Values() ([]any, error) {
	return r.rows.Values()
}

func (r *scopedRows) RawValues() [][]byte {
	return r.rows.RawValues()
}

func (r *scopedRows) Conn() *pgx.Conn {
	return r.rows.Conn()
}

func (s *ScopedDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	rows, err := s.Query(ctx, sql, args...)
	return &scopedRow{rows: rows, err: err}
}

type scopedRow struct {
	rows pgx.Rows
	err  error
}

func (r *scopedRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer r.rows.Close()
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return pgx.ErrNoRows
	}
	return r.rows.Scan(dest...)
}
