# Ticket 23: Track R.7 — Gate 07: Schema Migration Freshness, Upgrades & Lock Safety

- **Type**: `wayfinder:task`
- **Status**: Open
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Specification**: [Test Readiness Specification](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/TEST_READINESS_SPECIFICATION.md)
- **Dependencies**: None
- **Tier**: PR Gate (< 10 min)

## Objective

Validate database migration integrity, idempotency, upgrade path continuity, and lock safety.
Ensure all migrations (001 to 044) apply cleanly on a blank database.
Ensure re-running the migration runner does not error or cause unintended mutations.
Detect unsafe SQL operations that acquire prolonged exclusive table locks.

## Technical Context

Migrations reside in `migrations/` as numbered SQL files (`001_initial.sql` to `044_...sql`).
`cmd/migrate` applies pending migrations inside a PostgreSQL transaction using advisory locks.
Neon and PgBouncer pooled connections require strict single-statement transactions and explicit locks.
Unsafe DDL (e.g. unindexed foreign keys, default values on nullable columns without `NOT VALID`) can lock production tables.

## Karpathy Test Discipline

1. **Look at data first:**
   Inspect migration checksums in `schema_migrations` table.
   Review SQL statements in all 44 migration files.

2. **Check state at initialization:**
   Drop and recreate a blank test database `pg_migration_test`.
   Assert `information_schema.tables` contains zero application tables.

3. **Overfit one example:**
   Run `cmd/migrate` from 001 to 044 on the blank database.
   Verify that all tables, indexes, and constraints exist.

4. **Compare with dumb baseline:**
   Dump schema using `pg_dump --schema-only`.
   Compare the resulting schema against a verified reference schema.

5. **Fix seeds:**
   Deterministic schema verification.

6. **Change one thing at a time:**
   Test fresh apply first.
   Test re-run idempotency second.
   Test DDL lock linting third.

## STE-100 Implementation Steps

1. Create test file `internal/postgres/migration_test.go`.
2. Test 1 (Fresh Migration):
   - Connect to a clean database container.
   - Execute `cmd/migrate`.
   - Assert exit code 0.
   - Assert 44 migration rows exist in `schema_migrations`.
3. Test 2 (Idempotency):
   - Re-run `cmd/migrate` immediately on the migrated database.
   - Assert exit code 0 and zero errors.
   - Assert no rows or columns are duplicated or altered.
4. Test 3 (Upgrade Path):
   - Apply migrations up to revision 030.
   - Run `cmd/migrate` to upgrade to revision 044.
   - Assert all upgrade scripts execute cleanly.
5. Test 4 (Lock Safety Linting):
   - Integrate `squawk` or a custom SQL scanner.
   - Scan migration files for dangerous patterns:
     - `ALTER TABLE ... ADD COLUMN` without default optimization.
     - `CREATE INDEX` without `CONCURRENTLY` on populated tables.
     - Missing statement timeouts.

## Acceptance Criteria

- All migrations apply cleanly from 001 to 044 on an empty database.
- Running migrations on an already migrated database exits 0 with no modifications.
- Upgrading from older revisions completes without data loss or transaction aborts.
- Zero unsafe exclusive lock patterns detected in new migration files.
