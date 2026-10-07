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

// ARCHITECTURAL CONVENTION: DBTX vs. Pool-Only vs. Dual-Mode Repositories
//
// Repositories in this package adhere to three verified transactional models:
//
// 1. Single-Query / Delegated Repositories (DBTX):
//    - Examples: DueRepo, TenantRepo, OutboxRepo.
//    - Constructor: NewXYZRepo(db DBTX).
//    - Verified invariant: Methods execute at most one mutating statement (Exec/QueryRow).
//      They do NOT manage transactions internally; atomicity across multiple operations
//      is the sole responsibility of the caller (e.g., via postgres.WithinTx or an outer pgx.Tx).
//
// 2. Pool-Only Repositories (Compile-time Atomicity):
//    - Example: FinanceRepo.
//    - Constructor: NewFinanceRepo(pool *pgxpool.Pool).
//    - Invariant: When a repository never participates in an outer transaction, it accepts
//      ONLY *pgxpool.Pool at the constructor boundary. Multi-statement methods (InsertJournal,
//      SaveUnifiedSettings, EnsureDefaults) execute directly inside WithinTx(ctx, r.pool, ...).
//      This guarantees compile-time safety and prevents silent non-transactional degradation.
//
// 3. Dual-Mode Repositories (Pool or Transactional Context):
//    - Examples: PaymentIntentRepo, PaymentRepo.
//    - Constructor: NewXYZRepo(db DBTX) + WithTx(tx pgx.Tx).
//    - Invariant: When multi-write methods (e.g., PaymentIntentRepo.CreateWithDues,
//      PaymentRepo.Create) can be invoked standalone from a service OR inside an existing
//      caller transaction, they MUST explicitly handle both modes safely:
//          if pool, ok := r.db.(*pgxpool.Pool); ok {
//              return WithinTx(ctx, pool, func(tx pgx.Tx) error { return op(r.WithTx(tx)) })
//          }
//          return op(r) // executes on outer caller's tx; atomicity guaranteed by caller
//      NEVER provide an unguarded fallback that silently executes multi-statement writes without a transaction.

// DBTX is satisfied by *pgxpool.Pool and pgx.Tx.
type DBTX interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type txContextKey struct{}

// ContextWithTx stores a pgx.Tx in the context for propagation into downstream transactions.
func ContextWithTx(ctx context.Context, tx pgx.Tx) context.Context {
	return context.WithValue(ctx, txContextKey{}, tx)
}

// TxFromContext retrieves an active pgx.Tx from the context if present.
func TxFromContext(ctx context.Context) (pgx.Tx, bool) {
	tx, ok := ctx.Value(txContextKey{}).(pgx.Tx)
	return tx, ok
}

// WithinTx runs fn inside a transaction. Commits on nil error; rolls back otherwise.
// If the context already carries an active pgx.Tx via ContextWithTx, fn is executed directly
// within that existing transaction to guarantee cross-service atomicity without duplicate connections.
func WithinTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	if existingTx, ok := TxFromContext(ctx); ok {
		return fn(existingTx)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if propID, ok := requestscope.PropertyIDFromContext(ctx); ok && propID != uuid.Nil {
		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", propID.String()); err != nil {
			return fmt.Errorf("set local app.current_property_id: %w", err)
		}
	}

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
