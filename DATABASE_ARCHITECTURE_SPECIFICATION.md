# Database Architecture Specification & Remediation Traceability Matrix
## Repository: `pg-go-div_dev` · Standard: ASD-STE100 · Method: Empirical First-Principles Verification

---

## 1. Executive Summary

This document specifies the technical implementation and empirical verification for the database architecture of `pg-go-div_dev`.
All requirements follow **ASD-STE100 (Simplified Technical English)**.
All implementations follow **first-principles verification** on live PostgreSQL.

### Key Remediation Results
1. **Least-Privilege Roles & Scoped Architecture:**
   - Dedicated login roles: `pgapp_app` (`NOSUPERUSER NOBYPASSRLS`) for authenticated client queries, and `pgapp_maint` (`NOSUPERUSER NOBYPASSRLS`) for background tasks and webhooks.
   - `ScopedDB` enforces `set_config('app.current_property_id', ...)` in single round-trip batches.
   - RLS policies apply strictly to `pgapp_app`. Maintenance processes run on `pgapp_maint` with `USING (true)` and enforce property boundaries in application logic.
2. **Atomic Billing & Money-Correctness Hardening:**
   - `DueRepo.Create` executes `INSERT ... ON CONFLICT (due_code) DO NOTHING RETURNING id`. A collision returns `qr.ErrConflict` and does not abort the transaction.
   - Billing establishes a tenant-first row lock (`GetByIDForUpdate`) and applies credit atomically with `DeductCredit` (`UPDATE tenants SET credit_balance_paise = GREATEST(0, credit_balance_paise - $2)`), preventing lost updates from concurrent webhooks or reward redemptions.
3. **Production Startup Verification:**
   - Server startup validates the current role of `maintPool`. The server refuses startup in production if `maintPool` connects as `pgapp_app`.
4. **Gamification Monetary Protection:**
   - Cash credit reward redemptions tie the discount paise directly to the property point value (`discountPaise <= points_cost * point_value_paise`), preventing money leaks from metadata errors.
5. **Expense Void & Reversing Double-Entry Journal:**
   - Implemented `VoidExpense` action and endpoint (`POST /finance/expenses/:id/void`) transitioning expense status to `cancelled`.
   - Reversing double-entry journal lines are inserted atomically for approved expenses (Debit `accounts_payable`, Credit `operating_expense`).
6. **SQL Test Harness Repair:**
   - Repaired `scripts/sql/ledger_controls_test.sql` fixture insert on `payments` by providing the required `property_id` NOT NULL value.

---

## 2. Master Requirements Traceability Matrix

| Requirement ID | Severity | Category | Description | Artifacts & Code Scope | Verification Target | Status |
| :--- | :--- | :--- | :--- | :--- | :--- | :--- |
| **REQ-DB-P0-001** | P0 | Security / Isolation | Enforce least-privilege database roles (`pgapp_app`, `pgapp_maint`) with `NOSUPERUSER` and `NOBYPASSRLS`. | `migrations/057_...sql`, `migrations/058_...sql` | `TestLabA_RLSIsolationFailure/Subtest_1` | `PASS` |
| **REQ-DB-P0-002** | P0 | Security / Isolation | Wire `ScopedDB` into application entry point and HTTP auth middleware. | `cmd/server/main.go`, `internal/auth/middleware.go`, `internal/api/handlers_public.go` | `TestLabA_RLSIsolationFailure/Subtest_6` | `PASS` |
| **REQ-DB-P0-003** | P0 | Security / Isolation | Unscoped database requests under `pgapp_app` must fail closed and return zero rows. | `internal/postgres/scoped_dbt.go`, `migrations/057_...sql`, `migrations/058_...sql` | `TestLabA_RLSIsolationFailure/Subtest_3` | `PASS` |
| **REQ-DB-P0-004** | P0 | Security / Isolation | Enable dedicated LOGIN roles and remove obsolete `SET LOCAL ROLE` commands. | `migrations/058_...sql`, `internal/postgres/scoped_dbt.go`, `internal/postgres/dbtx.go` | Schema inspection and ScopedDB execution | `PASS` |
| **REQ-DB-P0-005** | P0 | Architecture / Routing | Support dual connection pools (`DATABASE_URL` for app, `DATABASE_MAINT_URL` for maint). | `internal/config/config.go`, `cmd/server/main.go`, `cmd/*` | Server and job initialization | `PASS` |
| **REQ-DB-P0-006** | P0 | Financial Integrity | Execute `CreateRentDue` and credit deduction within a single atomic transaction block. | `internal/billing/service.go`, `cmd/server/main.go`, `cmd/billing-cycle/main.go` | Transactional rollback verification | `PASS` |
| **REQ-DB-P0-007** | P0 | Concurrency / Integrity | Use `ON CONFLICT (due_code) DO NOTHING` to prevent aborted transactions on due code collision. | `internal/postgres/due_repo.go`, `internal/billing/service.go` | Code review and in-transaction retry check | `PASS` |
| **REQ-DB-P0-008** | P0 | Concurrency / Integrity | Prevent lost tenant credit updates via `GetByIDForUpdate` and atomic `DeductCredit`. | `internal/postgres/property_repo.go`, `internal/billing/service.go` | Tenant lock ordering and atomic decrement check | `PASS` |
| **REQ-DB-P0-009** | P0 | Operational Safety | Refuse server startup in production if `maintPool` connects as restricted `pgapp_app`. | `cmd/server/main.go` | Startup role verification query | `PASS` |
| **REQ-DB-P1-001** | P1 | Performance / Indexing | Eliminate `OR` branch in RLS policies to allow index condition pushdown. | `migrations/057_...sql`, `migrations/058_...sql` | `TestLabB_PoolSaturationAndQueryLatency/Subtest_1` | `PASS` |
| **REQ-DB-P1-002** | P1 | Security / Integrity | Cover all financial tables (`financial_journal_entries`, `bank_transactions`, etc.) with forced RLS. | `migrations/057_...sql`, `migrations/058_...sql` | Migration 057/058 schema verification | `PASS` |
| **REQ-DB-P1-003** | P1 | Correctness / Concurrency | Consume `COMMIT` across all `ScopedDB` operations to guarantee connection pool reuse. | `internal/postgres/scoped_dbt.go` | `TestScopedDB_BatchedPipelining` | `PASS` |
| **REQ-DB-P1-004** | P1 | Performance / Indexing | Add composite index `(property_id, due_date DESC, id DESC)` for keyset pagination. | `migrations/057_...sql` | `TestLabB_PoolSaturationAndQueryLatency/Subtest_1` | `PASS` |
| **REQ-DB-P1-005** | P1 | Financial Integrity | Prevent accidental cascade deletes on ledger-adjacent tables with `ON DELETE RESTRICT`. | `migrations/057_...sql` | Migration 057 constraint check | `PASS` |
| **REQ-DB-P1-006** | P1 | Security / Authorization | Prevent non-maintenance sessions from bypassing RLS via session GUC settings. | `migrations/057_...sql`, `migrations/058_...sql` | `TestLabA_RLSIsolationFailure/Subtest_5` | `PASS` |
| **REQ-DB-P1-007** | P1 | Security / Integrity | Revoke `TRUNCATE` from runtime roles and enforce append-only rules on ledger entries. | `migrations/058_...sql` | Database privilege inspection | `PASS` |
| **REQ-DB-P1-008** | P1 | Performance / Hygiene | Drop redundant indexes (`idx_dues_prop_due_date`, duplicate balances/rollups) and validate check constraints. | `migrations/058_...sql` | Database index and constraint inspection | `PASS` |
| **REQ-DB-P1-009** | P1 | Maintenance / Storage | Schedule automated image purge for expired payment proof images (>30 days). | `cmd/server/main.go`, `internal/postgres/report_repo.go` | Background ticker verification | `PASS` |
| **REQ-DB-P1-010** | P1 | Financial Integrity | Tie gamification cash redemption to point value (`discountPaise <= points_cost * point_value_paise`). | `internal/gamification/redemptions.go` | Redemptions monetary check | `PASS` |
| **REQ-DB-P1-011** | P1 | Financial Controls | Implement `VoidExpense` action and endpoint with reversing double-entry journal lines. | `internal/finance/service.go`, `internal/api/handlers_finance.go`, `internal/api/router.go` | Reversing journal verification | `PASS` |
| **REQ-DB-P1-012** | P1 | Test Automation | Fix missing `property_id` in `scripts/sql/ledger_controls_test.sql` payments fixture. | `scripts/sql/ledger_controls_test.sql` | SQL control test execution | `PASS` |
| **REQ-DB-P0-013** | P0 | Concurrency / Integrity | Block resurrection of voided expenses by cancelling pending approvals and guarding `DecideApprovalAtomic`. | `internal/postgres/finance_repo.go`, `internal/finance/mem.go` | `TestVoidPendingExpenseCannotBeApprovedLater` | `PASS` |
| **REQ-DB-P0-014** | P0 | Concurrency / Integrity | Lock expense row with `FOR UPDATE` inside transaction before making void and journal reversal decisions. | `internal/postgres/finance_repo.go`, `internal/finance/service.go` | In-transaction row lock inspection | `PASS` |
| **REQ-DB-P1-015** | P1 | Audit / Integrity | Add audit columns (`void_reason`, `voided_by`, `voided_at`) and check constraint to `expenses`. | `migrations/059_expense_void_audit.sql`, `internal/domain/finance.go` | Migration 059 schema inspection | `PASS` |
| **REQ-DB-P1-016** | P1 | Financial Integrity | Fail closed on settings errors during cash credit reward redemptions. | `internal/gamification/redemptions.go` | Redemptions error handling check | `PASS` |
| **REQ-DB-P1-017** | P1 | Performance / Indexing | Replace `TO_CHAR` with IST range queries and add covering indexes on `points_ledger`. | `migrations/060_points_ledger_month_indexes.sql`, `internal/postgres/gamification_repo.go` | Query plan and index scan verification | `PASS` |
| **REQ-DB-P1-018** | P1 | Test Automation | Add comprehensive unit tests covering journal netting, resurrection blocking, and maker-checker. | `internal/finance/void_test.go` | `go test -run TestVoid` | `PASS` |

---

## 3. ASD-STE100 Technical Specifications

### REQ-DB-P0-007: Due-Code Collision Handling in Transaction Block
- **Statement:** The system shall insert dues without aborting the transaction when a due code collision occurs.
- **Acceptance Criteria:**
  1. `DueRepo.Create` shall execute `INSERT INTO dues (...) ON CONFLICT (due_code) DO NOTHING RETURNING id`.
  2. When zero rows are returned, `DueRepo.Create` shall return `qr.ErrConflict`.
  3. PostgreSQL shall not mark the transaction as aborted.
  4. `qr.GenerateDueCode` shall generate a new code and retry insertion within the same transaction.

### REQ-DB-P0-008: Concurrency Protection for Tenant Credit
- **Statement:** The billing service shall lock the tenant row and apply credit balance atomically.
- **Acceptance Criteria:**
  1. `CreateRentDue` shall call `txTenants.GetByIDForUpdate` inside the transaction before modifying due records.
  2. Tenant credit deduction shall execute `UPDATE tenants SET credit_balance_paise = GREATEST(0, credit_balance_paise - $2) RETURNING credit_balance_paise`.
  3. Concurrent additions from payment webhooks or reward redemptions shall not be overwritten.

### REQ-DB-P0-009: Startup Environment Validation
- **Statement:** The application server shall refuse startup in production if connection roles violate security boundaries or if pools share the same connection string.
- **Acceptance Criteria:**
  1. `cmd/server/main.go` and `internal/postgres/guard.go` shall validate role metadata (`rolname`, `rolsuper`, `rolbypassrls`, table ownership) on both pools.
  2. If `APP_ENV` is `production`:
     - `DATABASE_URL` and `DATABASE_MAINT_URL` must differ.
     - `maintPool` role must be `pgapp_maint`, NOSUPERUSER, NOBYPASSRLS, and not a public table owner.
     - `appPool` role must be `pgapp_app`, NOSUPERUSER, NOBYPASSRLS, and not a public table owner.
  3. If misconfigured, startup terminates immediately with `log.Fatal`.

### REQ-DB-P1-010: Cash Redemption Monetary Limits and Fail-Closed Control
- **Statement:** The gamification engine shall limit cash credit discounts to the total value of spent points and fail closed on missing settings.
- **Acceptance Criteria:**
  1. `RedeemReward` shall load property settings and refuse redemptions if settings cannot be loaded or if `PointValuePaise <= 0`.
  2. If metadata specifies a discount higher than `PointsCost * PointValuePaise`, the discount shall be clamped to `PointsCost * PointValuePaise`.

### REQ-DB-P1-011: Expense Cancellation, Role Authorization, and Concurrency Protection
- **Statement:** The finance service shall permit cancellation of unpaid expenses with atomic journal reversal, role checks, and race-free conditional updates.
- **Acceptance Criteria:**
  1. `VoidExpense` shall require a non-empty `void_reason` and persist `void_reason`, `voided_by`, and `voided_at` on the `expenses` record.
  2. Managers may void only their own draft or pending expenses. Voiding approved expenses is restricted to the owner role.
  3. The update query shall enforce `AND status = $5` matching the status read before the call. If zero rows match or a concurrent payment lands, the call shall return `ErrConflict`.
  4. If the expense was approved, a balanced reversing journal entry shall be recorded: Debit `accounts_payable`, Credit `operating_expense`.

---

## 4. Architectural Readiness Verdict

| Dimension | Initial Review Finding | Current Post-Remediation State | Verdict |
| :--- | :--- | :--- | :--- |
| **Tenant Isolation** | Scoped queries failed under superuser tests; unscoped routes broke. | Dual pools deployed: `pgapp_app` for scoped client queries, `pgapp_maint` for background operations. | **VERIFIED SECURE** |
| **Billing Atomicity** | Aborted transaction on collision; lost update on credit balance. | `ON CONFLICT DO NOTHING` + tenant-first lock hierarchy + atomic `DeductCredit`. Tested under concurrent webhooks and collision loops. | **CORRECT & ATOMIC** |
| **Startup Safety** | Startup guard allowed table-owner, superuser, and identical URLs. | Strict guard validates `pgapp_maint` and `pgapp_app` roles, non-superuser, non-bypassrls, non-tableowner, and distinct URLs. | **HARDENED** |
| **Gamification Leak** | Redemption failed open on settings errors or `PointValuePaise <= 0`. | Fails closed with error on unconfigured settings; clamps excessive metadata discounts to `PointsCost * PointValuePaise`. | **PROTECTED** |
| **Financial Controls** | Void raced with payments; managers could void approved expenses. | `AND status = $5` concurrency lock with `ErrConflict`; manager void limited to own draft/pending; approved void is owner-only. | **CONTROLLED** |
| **Test Fixtures & Suite** | Missing billing and void race Go tests. | Comprehensive unit tests added in `billing/service_test.go`, `finance/void_test.go`, and `postgres/guard_test.go`. | **PASS** |
