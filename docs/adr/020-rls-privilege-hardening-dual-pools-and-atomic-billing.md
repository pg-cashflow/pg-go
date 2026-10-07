# ADR-020: RLS Privilege Hardening, Dual Connection Pools, Atomic Billing, and Financial Controls

**Status:** Accepted  
**Date:** 2026-10-07  
**Deciders:** Engineering Lead (owner of pg-go)  
**Builds on:** ADR-015, ADR-017, ADR-018, ADR-019  
**Standard:** ASD-STE100 (Simplified Technical English)  
**Method:** Empirical First-Principles Verification  

---

## 1. Context

Code analysis and database execution identified operational risks and money-correctness bugs:
1. `SET LOCAL ROLE pgapp_app` failed with permission denied for non-superuser login roles because role membership was missing.
2. Direct connection role usage separated application traffic (`pgapp_app`) from maintenance tasks (`pgapp_maint`).
3. If production configured `DATABASE_MAINT_URL` with `pgapp_app`, webhooks and public payment pages returned zero rows.
4. `CreateRentDue` had two transaction bugs:
   - A `due_code` collision aborted the transaction in PostgreSQL. Later statements failed immediately.
   - `TenantRepo.Update` wrote an absolute `credit_balance_paise` value read before transaction start. Concurrent webhooks or reward redemptions lost credit updates.
5. Reward redemptions did not enforce a link between cash discount paise and point value. Catalog data errors could cause monetary loss.
6. The finance service lacked an expense void operation. Approved expenses with errors required manual database intervention.
7. RLS policies apply only to `pgapp_app`. Services using `pgapp_maint` use policy `USING (true)`.

---

## 2. Decisions

### D1. Migration 058 (Privilege Hardening and Role-Targeted Policies)
- Convert `pgapp_app` and `pgapp_maint` to login roles (`NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB`).
- Revoke `TRUNCATE` across all application tables. Revoke `UPDATE, DELETE` on `financial_journal_entries`.
- Restrict property-isolation policies explicitly `TO pgapp_app`.
- Drop redundant indexes: `idx_dues_prop_due_date`, `idx_daily_settlement_balances_prop_date`, `idx_daily_financial_rollups_prop_date`.
- Validate all check constraints added with `NOT VALID`.

### D2. Dual-Connection Architecture and Production Startup Guard
- Configure `DATABASE_URL` for authenticated endpoints using `pgapp_app`.
- Configure `DATABASE_MAINT_URL` for maintenance tasks, webhooks, and public routes using `pgapp_maint`.
- Clarify isolation boundary: RLS protects scoped repositories. Maintenance services run with `USING (true)` and enforce property boundaries in application logic.
- Enforce startup verification in `cmd/server/main.go`. In production, the server stops immediately if `maintPool` connects as `pgapp_app`.

### D3. Safe Due-Code Generation and Atomic Rent Billing
- In `DueRepo.Create`, execute `INSERT ... ON CONFLICT (due_code) DO NOTHING RETURNING id`.
- If zero rows return, return `qr.ErrConflict`. The transaction does not abort.
- In `CreateRentDue`, lock the tenant row first with `txTenants.GetByIDForUpdate(ctx, tenant.ID)`.
- Apply credit balance using atomic decrement: `UPDATE tenants SET credit_balance_paise = GREATEST(0, credit_balance_paise - $2) RETURNING credit_balance_paise`.

### D4. Gamification Cash Redemption Monetary Protection
- In `RedeemReward`, validate `cash_credit` discounts against the property point value.
- Enforce `discountPaise <= points_cost * point_value_paise`.

### D5. Expense Void and Journal Reversal
- Add `VoidExpense` in the finance service and expose `POST /finance/expenses/:id/void`.
- Set expense status to `cancelled`.
- For approved expenses, insert a reversing journal entry atomically:
  - Debit: `accounts_payable`
  - Credit: `operating_expense`

### D6. Automated Image Purge
- Schedule `PurgeExpiredImages` daily in `cmd/server/main.go`.
- Purge images older than 30 days using `DATABASE_MAINT_URL`.

---

## 3. Consequences

### Positive
- `ON CONFLICT DO NOTHING` prevents transaction abort on due-code collisions.
- Tenant row lock and atomic credit deduction prevent lost credit balances.
- Production startup stops if maintenance pool credentials are misconfigured.
- Cash reward redemptions cannot exceed point values.
- Operators can cancel mistaken expenses safely with double-entry journal reversal.
- Financial journal entries remain immutable.

### Operational Notice
- Multi-property tenant isolation in maintenance workers depends on application checks, not database RLS.

---

## 4. Verification Matrix

| Target | Description | Status |
| :--- | :--- | :--- |
| `migrations/058_rls_hardening_and_index_cleanup.sql` | Applied privilege hardening, policy targeting, index drops, constraint validation | VERIFIED |
| `internal/postgres/due_repo.go` | Added `ON CONFLICT (due_code) DO NOTHING` to prevent aborted transactions | VERIFIED |
| `internal/postgres/property_repo.go` | Added atomic `DeductCredit` with `GREATEST(0, credit - $2)` | VERIFIED |
| `internal/billing/service.go` | Added tenant lock hierarchy and atomic credit deduction in `CreateRentDue` | VERIFIED |
| `internal/gamification/redemptions.go` | Capped cash redemption discount to `points_cost * point_value_paise` | VERIFIED |
| `internal/finance/service.go` | Implemented `VoidExpense` with reversing journal lines | VERIFIED |
| `internal/api/handlers_finance.go` | Added `VoidExpense` handler for owner and manager routes | VERIFIED |
| `cmd/server/main.go` | Added production startup role check on `maintPool` | VERIFIED |
| `scripts/sql/ledger_controls_test.sql` | Added `property_id` NOT NULL column to `payments` fixture | VERIFIED |
