package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
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

// WithinTx runs fn inside a transaction. Commits on nil error; rolls back otherwise.
func WithinTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit tx: %w", err)
	}
	return nil
}
