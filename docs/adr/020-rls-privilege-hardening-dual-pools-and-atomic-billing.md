# ADR-020: RLS Privilege Hardening, Dual Connection Pools, Redundant Index Cleanup, and Atomic Billing

**Status:** Accepted  
**Date:** 2026-10-07  
**Deciders:** Engineering Lead (owner of pg-go)  
**Builds on:** ADR-015, ADR-017, ADR-018, ADR-019  
**Standard:** ASD-STE100 (Simplified Technical English)  
**Method:** Empirical First-Principles Verification  

---

## 1. Context

Code and database review identified four operational risks in the previous update:
1. `SET LOCAL ROLE pgapp_app` failed with permission denied for non-superuser login roles because role membership was not granted.
2. Unscoped queries returned zero rows, breaking the tenant magic-link public payment page, webhooks, and finance background workers.
3. The application role possessed `TRUNCATE` privileges on core financial tables, permitting cascade truncation of journal entries.
4. `CreateRentDue` inserted the due, published events, and deducted tenant credit across separate uncoordinated steps without a wrapping transaction.
5. Payment proof images were never purged because `PurgeExpiredImages` lacked a background scheduler in `cmd/server/main.go`.
6. Three redundant indexes added unnecessary write overhead.
7. Seven check constraints were added `NOT VALID` and never validated.

---

## 2. Decisions

### D1. Migration 058 (Privilege Hardening and Role-Targeted Policies)
- Convert `pgapp_app` and `pgapp_maint` to real login roles (`LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB`).
- Revoke `ALL` privileges and grant only `SELECT, INSERT, UPDATE, DELETE` on application tables.
- Revoke `UPDATE, DELETE` on `financial_journal_entries` for runtime roles to guarantee append-only immutability.
- Restrict property-isolation policies explicitly `TO pgapp_app`, preventing multi-policy disjunctions on `pgapp_maint`.
- Drop redundant indexes:
  - `idx_dues_prop_due_date` (superseded by keyset index `idx_dues_prop_due_date_id`).
  - `idx_daily_settlement_balances_prop_date` (duplicate of unique constraint).
  - `idx_daily_financial_rollups_prop_date` (duplicate of unique constraint).
- Validate all unvalidated check constraints across catalog tables.

### D2. Dual-Connection Architecture (`DATABASE_URL` and `DATABASE_MAINT_URL`)
- Configure `DATABASE_URL` for authenticated client API requests connecting as `pgapp_app`.
- Configure `DATABASE_MAINT_URL` for maintenance processes connecting as `pgapp_maint`.
- Route public payment routes (`/p/:token`), payment webhooks, authentication/OTP services, finance mirror workers, and `cmd/*` cron jobs through `DATABASE_MAINT_URL`.
- Route authenticated, property-scoped repository operations through `DATABASE_URL` wrapped by `ScopedDB`.
- Remove `SET LOCAL ROLE pgapp_app` from `scoped_dbt.go` and `dbtx.go`. Direct role connection eliminates session role switching.

### D3. Atomic Rent Due Creation
- Add `NewServiceWithPool` and `SetPool` to `internal/billing/service.go`.
- Execute due code generation, due insertion, due event creation, and tenant credit deduction in a single transaction block via `WithinTx`.
- Guarantee that any mid-flight error or system crash rolls back both due record and credit deduction.

### D4. Automated Payment-Proof Image Purge
- Schedule `PurgeExpiredImages` inside the daily maintenance loop in `cmd/server/main.go`.
- Purge payment report images older than 30 days using `DATABASE_MAINT_URL`.

---

## 3. Consequences

### Positive
- Production deployment will not fail with permission denied on `SET LOCAL ROLE`.
- Public payment pages resolve due records successfully without requiring property-scoped session tokens.
- Truncate attacks and accidental cascades against the financial journal are blocked at the database privilege layer.
- Rent due creation and credit deduction are strictly atomic.
- Unused indexes are eliminated, reducing table write amplification.
- Image storage does not grow indefinitely in the primary database.

### Negative / Operational
- Operators must provision two sets of credentials (`pgapp_app` and `pgapp_maint`) in production environments.
- Fallback logic defaults `DATABASE_MAINT_URL` to `DATABASE_URL` when unset to preserve single-database developer setups.

---

## 4. Verification Matrix

| Target | Description | Status |
| :--- | :--- | :--- |
| `migrations/058_rls_hardening_and_index_cleanup.sql` | Applied privilege restriction, policy targeting, index drops, constraint validation | VERIFIED |
| `internal/config/config.go` | Added `DatabaseMaintURL` with fallback to `DatabaseURL` | VERIFIED |
| `internal/postgres/scoped_dbt.go` | Removed `SET LOCAL ROLE` from `Exec`, `Query`, `QueryRow` | VERIFIED |
| `internal/postgres/dbtx.go` | Removed `SET LOCAL ROLE` from `WithinTx` | VERIFIED |
| `internal/billing/service.go` | Added transactional execution wrapping due creation and credit application | VERIFIED |
| `cmd/server/main.go` | Wired dual connection pools, maintenance repos, and image purge scheduler | VERIFIED |
| `cmd/*` cron jobs | Configured `DatabaseMaintURL` across all background tasks | VERIFIED |
