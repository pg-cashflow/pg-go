# ADR-019: Database Review: ScopedDB Defects, RLS Cost, Remaining Index Debt, Scaling Triggers

**Status:** Proposed
**Date:** 2026-10-06
**Deciders:** Engineering Lead (owner of pg-go)
**Builds on:** ADR-015, ADR-017, ADR-018

## Context

Review of `pg-go-div_dev` after the ADR-018 change set. Method: all 55 migrations applied in the
runner's order (sorted filename) to a fresh PostgreSQL 16.15; catalog queries; a synthetic dataset
(200 properties, 12,000 tenants, 288,000 dues); `EXPLAIN (ANALYZE)` runs as a superuser and as a
`NOSUPERUSER NOBYPASSRLS` role. Single runs on one machine: indicative, not production benchmarks.
The Go module proxy was unreachable from the review sandbox, so **no Go code was compiled or run**;
the Go findings come from reading the code and the pgx v5.10.0 source.

### What is sound (keep)
- Money is `BIGINT` paise everywhere; no float/numeric money columns. All timestamps are `timestamptz`.
- Every table has a primary key. 97 tables, ~313 indexes after 056.
- Idempotency and integrity live in the database: unique keys on `(source_type, source_id, line_kind)`,
  `uq_bank_transactions_prop_dedup`, `uq_settled_deposit_due`, append-only and balanced-journal triggers,
  period-lock triggers.
- Dues list already supports keyset pagination (`(due_date, id) < ($, $)`); OFFSET is only a fallback.
- Pool defaults are sensible (25 conns, statement/lock/idle-in-tx timeouts, slow-query tracer at 100 ms,
  separate search pool, PgBouncer auto-detect on port 6432).
- Tenant column list excludes `id_photo_bytes`; the blob is read only by an explicit query.

### Findings

**F1. `ScopedDB.Query` leaves the transaction open (High once wired; zero impact today).**
It queues `BEGIN`, `set_config`, the query, and never `COMMIT`/`ROLLBACK`. pgxpool (v5.10.0 `conn.go`
`Release`) destroys any connection whose `TxStatus() != 'I'`. So every scoped read would close and
re-open a connection, defeating the pool. The new test passes because it only checks the GUC did not leak
(it cannot leak on a destroyed connection). Add an assertion on `pool.Stat().NewConnsCount()`.

**F2. `ScopedDB.QueryRow` ends with `ROLLBACK` (High once wired; silent data loss).**
Any `INSERT/UPDATE ... RETURNING` issued through a scoped `QueryRow` is rolled back after the row is
returned. Use `COMMIT` for all three methods.

**F3. `ScopedDB` has no production callers.** `grep` finds none outside tests. RLS is therefore only
as real as the connection role: a bypass role means it protects nothing; a least-privilege role means every
unscoped query returns zero rows. (ADR-018 reached the same conclusion; F1/F2 mean the fix must not ship as is.)

**F4. The RLS policy shape costs 15x on the hottest query.** Measured, app role, 1,440 dues in the
property, `WHERE property_id=$1 ORDER BY due_date DESC, id DESC LIMIT 50`:

| Setup | Plan | Time |
|---|---|---|
| Superuser (RLS bypassed) | Index scan backward + incremental sort, 61 rows read | 0.39 ms |
| Current `... OR ledger_maintenance = 'on'` policy | Bitmap scan of all 1,440 rows, policy as per-row Filter, top-N sort | 6.1 ms |
| Role-targeted policy (Option C below) | Index scan backward, policy as one-time filter | 0.45 ms |

Without an explicit property predicate (pending dues, one quarter): 20.1 ms (current policy, scans 24,000
rows to keep 120) vs 1.26 ms (role-targeted). Cost grows with dues per property.

**F5. RLS covers 6 of 75 tables that carry `property_id`.** Unprotected: `financial_journal_entries`,
`bank_transactions`, `gateway_settlements`, `gateway_refunds`, `payout_batches`, `events`, `payment_reports`,
`notifications`, and others. The money tables with the most sensitive rows have no database-level isolation.

**F6. Two migrations are numbered 054.** Works (version key is the filename) but breaks the "one number =
one change" convention and ordering assumptions. Rename `054_index_dedup_and_autovacuum.sql` to
`055_...` (idempotent, safe to re-run).

**F7. 10 more redundant indexes remain after 054**, all prefix-covered by UNIQUE indexes (see migration 056).
Verified: 323 -> 313 indexes, migration idempotent, no Go/SQL reference to any dropped name.

**F8. `deposit_settlements` policy (054) is not index-friendly and can silently fail to install.** It
compares `current_setting(...) = property_id::text` (cast on the column) and the DO block wraps it in
`EXCEPTION WHEN OTHERS THEN NULL`. Fixed in 056.

**F9. Cascade deletes reach ledger-adjacent tables.** `expenses`, `bank_transactions`, `deposit_settlements`,
`payout_batches`, `ledger_outbox_events` are `ON DELETE CASCADE` from `properties` (and `deposit_settlements`
from `tenants`). Only `financial_journal_entries` has a delete-blocking trigger. The app never issues
`DELETE FROM properties|tenants|dues`, so this is a latent hazard (manual SQL, future admin tool), not a live bug.
Prefer `RESTRICT` plus the existing `archived_at` soft delete.

**F10. 85 foreign keys have no supporting index.** Most are `created_by`/`recorded_by` to `users`. Because the
app never deletes parent rows, the usual cost (seq scan of the child on parent delete) does not occur. **Do not
bulk-add these**: each index taxes writes. Revisit only if parent deletes are introduced.

## Decision

1. Fix `ScopedDB` before any wiring (snippet below), add the connection-reuse test.
2. Choose **Option C** from ADR-018 (role-targeted RLS), now with measured justification (F4), and extend it to
   the money tables (F5) in a second step, not all at once.
3. Rename the duplicate 054; ship migration 056.
4. Keep replicas/sharding (ADR-017) dormant; scale triggers below.

### ScopedDB fix (not compiled; review before use)

```go
// Exec / QueryRow / Query: all three end the batch with COMMIT (never ROLLBACK, never nothing).
batch.Queue("COMMIT")

// Query: consume COMMIT when rows are closed, so the connection returns idle ('I').
func (r *batchedScopedRows) Close() {
	r.rows.Close()
	_, _ = r.br.Exec() // COMMIT
	_ = r.br.Close()
}

// QueryRow: same, replace the final "consume ROLLBACK" with consuming COMMIT.
```

Test to add: run 200 scoped calls, assert `pool.Stat().NewConnsCount()` did not grow, and that an
`INSERT ... RETURNING` through `QueryRow` is visible from a second connection.

### Option C SQL (tested on PG16; sketch, per table)

```sql
CREATE ROLE pgapp_app  LOGIN NOSUPERUSER NOBYPASSRLS;
CREATE ROLE pgapp_maint LOGIN NOSUPERUSER NOBYPASSRLS;   -- cron jobs, workers, migrations
DROP POLICY due_property_isolation ON dues;
CREATE POLICY due_app   ON dues FOR ALL TO pgapp_app
  USING      (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
  WITH CHECK (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid);
CREATE POLICY due_maint ON dues FOR ALL TO pgapp_maint USING (true) WITH CHECK (true);
```
Tested: scope set -> own property only; GUC empty -> 0 rows, no cast error (fail-closed); maintenance role -> all
288,000 rows. Webhooks and other no-user paths must use the maintenance pool or set an explicit scope.

## Options Considered

| Option | Complexity | Isolation | Hot-query cost |
|---|---|---|---|
| A. Status quo (OR policy, bypass role) | None | None in practice | 15x if ever enforced |
| B. Drop RLS, app-layer scoping | Low | Review-dependent | None |
| C. Role-targeted RLS + scoped executor | Med-High | Database-enforced, fail-closed | ~1.1x |

Choose C if more than one owner will ever share this database; B is defensible for a single operator. A is not.

## Scaling plan and triggers (assumed 200 properties, 12k tenants: single-digit req/s)

- **Capacity is not the constraint.** Latency per request (round trips, app-to-DB distance) and correctness are.
- **Keyset pagination:** convert `settlements`, `bank_transactions`, `settlement_balancer` lists from OFFSET to
  `(sort_col, id)` cursors when any list exceeds ~5k rows per property. Dues already has it; make the cursor
  mandatory there and delete the OFFSET fallback.
- **Partitioning:** monthly range partitions for `events` and `financial_journal_entries` when either passes
  ~50M rows (not before). Note `uq_journal_source_line` would need the partition key; plan that unique-key
  change first.
- **Billing-cycle batching:** when `billing-cycle` runtime approaches its window, replace per-tenant
  `CreateRentDue` with one `INSERT ... SELECT ... ON CONFLICT DO NOTHING` per property.
- **Replicas:** only when p95 query time > 100 ms (existing tracer) or connections > 70% of `max_connections`.
- **Index hygiene:** enable `pg_stat_statements`; after two weeks review `pg_stat_user_indexes` (`idx_scan = 0`),
  especially `idx_events_type_trgm` (GIN on a low-cardinality column of the highest-write table).

## Consequences

- Easier: pooled connections stay pooled; RLS becomes enforceable and cheap; fewer write-path indexes.
- Harder: two roles/pools to operate; every code path needs an explicit scope or the maintenance role.
- Revisit: after RLS is wired, re-measure F4 on production-sized data.

## Action Items

1. [x] Fix F1/F2 in `scoped_dbt.go`; add the connection-reuse and write-visibility tests.
2. [x] Rename `054_index_dedup_and_autovacuum.sql` to `055_...`; add `056_drop_remaining_redundant_indexes.sql`.
3. [ ] Check production role (`\du`; `BYPASSRLS`?).
4. [ ] Decide B vs C; if C, wire middleware + two pools, stage on a Neon branch, then switch `DATABASE_URL`.
5. [ ] Extend RLS to journal, bank, gateway tables one at a time, re-running the plan checks.
6. [ ] Switch cascades to `RESTRICT` on ledger-adjacent tables.
7. [ ] Enable `pg_stat_statements`; schedule the index-usage review.
