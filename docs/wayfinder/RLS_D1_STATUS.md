# RLS D1 Status Audit: Money-Path and Sensitive Data Tables

**Status:** Completed  
**Date:** 2026-10-14  
**Standard:** ASD-STE100 (Simplified Technical English)  
**Method:** Empirical First-Principles Inspection (Karpathy Method)  
**References:** ADR-018, ADR-019, ADR-028, ADR-029, Migration 057, `scripts/sql/rls_property_scope_ratchet.sql`

---

## 1. Executive Summary

ADR-029 Decision 1 (D1) mandates Row-Level Security (RLS) and `ScopedDB` context binding for money paths and sensitive data.
This audit inspected the database schema (migrations 001 to 069), repository constructors in `internal/postgres/`, wiring in `cmd/server/main.go`, and test suites.

### Key Audit Findings:
1. **Schema vs Request-Path Disconnect:** 
   `bank_transactions`, `payout_batches`, and `financial_journal_entries` have PostgreSQL RLS enabled and forced by Migration 057. However, their repositories (`BankTransactionRepo`, `PayoutRepo`, `FinanceRepo`) receive `appPool` directly rather than `ScopedDB`. In production under `pgapp_app`, queries without `SET app.current_property_id` fail closed or rely on unverified session state.
2. **Unprotected Money Tables:** 
   `bank_accounts`, `payout_items`, `payout_records`, and `property_calendar_tokens` have no database RLS. They remain in the `_rls_baseline` debt register in `scripts/sql/rls_property_scope_ratchet.sql` and rely solely on application-layer `WHERE property_id = $1` filters.
3. **Missing Integration Isolation Tests:** 
   Cross-property live isolation tests exist for `tenants` and `financial_journal_entries`, but are absent for `bank_accounts`, `bank_transactions`, `payout_batches`, `payout_items`, and `property_calendar_tokens`.

---

## 2. Audit Matrix

| Target Entity / Table | Database RLS Enabled & Forced? | Policy Name & Predicate | Pool Wiring in Server | ScopedDB on Request Path? | Cross-Property Live Test Exists? |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **`bank_accounts`** | **YES** (Migration 086) | `bank_accounts_app`, `bank_accounts_maint` | `scopedDB` (`postgres.NewBankAccountRepo`) | **YES** | **YES** (`bank_account_repo_test.go`) |
| **`bank_transactions`** | **YES** (Migration 057) | `bank_transactions_app`, `bank_transactions_maint` | `scopedDB` (`postgres.NewBankTransactionRepo`) | **YES** | **YES** (`bank_transaction_repo_test.go`) |
| **`payout_batches`** | **YES** (Migration 057) | `payout_batches_app`, `payout_batches_maint` | `scopedDB` (`postgres.NewPayoutRepo`) | **YES** | **YES** (`payout_batch_isolation_test.go`) |
| **`payout_items`** | **YES** (Migration 087) | `payout_items_app` (via `payout_payees`), `payout_items_maint` | `scopedDB` (`postgres.NewPayoutRepo`) | **YES** | **YES** (`payout_batch_isolation_test.go`) |
| **`payout_records`** | **YES** (Migration 087) | `payout_records_app`, `payout_records_maint` | `scopedDB` (`postgres.NewPayoutRepo`) | **YES** | **YES** |
| **`financial_journal_entries`** | **YES** (Migration 057) | `financial_journal_entries_app`, `financial_journal_entries_maint` | `scopedDB` (`postgres.NewFinanceRepo`) | **YES** | **YES** (`finance_repo_test.go`, `step3_maint_rls_live_test.go`) |
| **`property_calendar_tokens`** | **YES** (Migration 085) | `property_calendar_tokens_app`, `property_calendar_tokens_maint` | `scopedDB` (`postgres.NewCalendarTokenRepo`) | **YES** | **YES** (`calendar_token_repo_test.go`) |
| **`tenants`** (Identity Fields) | **YES** (Row-level RLS, Migration 057) | `tenants_app` (`property_id = NULLIF(current_setting('app.current_property_id', true), '')::UUID`), `tenants_maint` (`TO pgapp_maint USING (true)`) | `scopedDB` (`postgres.NewTenantRepo(scopedDB)`) | **YES** (for `tenantRepo`; **NO** for `maintTenantRepo`) | **YES** (`lab_a_rls_isolation_test.go`, `rls_live_test.go`) |

*Note on `tenants` Identity Fields: Row isolation is enforced, but sensitive fields (`phone`, `aadhaar_last4`, `guardian_phone`, `guardian_kyc_reference_id`) lack column-level encryption or projection masking.*

---

## 3. Follow-Up Engineering Tickets (Pre-M4 Execution)

Every "NO" in the audit matrix above generates a prioritized, sized follow-up ticket:

### Ticket R9-F1: Extend RLS and Wire ScopedDB for `bank_accounts` [COMPLETED]
- **Scope:** 
  1. Add migration enabling and forcing RLS on `bank_accounts` with `bank_accounts_app` and `bank_accounts_maint` policies.
  2. Remove `bank_accounts` from `_rls_baseline` in `scripts/sql/rls_property_scope_ratchet.sql`.
  3. Refactor `BankAccountRepo` to accept `ScopedDB` or enforce scoped execution.
  4. Add unit test with `ScopedDB` in `internal/postgres/bank_account_repo_test.go`.
- **Status:** Complete. Migration 086 applied, `BankAccountRepo` refactored to `DBTX`, wired via `ScopedDB` in `bootstrap.go`, covered by unit tests.
- **Size:** M (Medium, 1 day)

### Ticket R9-F2: Wire ScopedDB and Add Cross-Property Live Test for `bank_transactions` [COMPLETED]
- **Scope:**
  1. Refactor `BankTransactionRepo` constructors and query execution to use `ScopedDB`.
  2. Ensure owner dashboard banking endpoints propagate property context to transaction queries.
  3. Author `internal/postgres/bank_transaction_repo_test.go` verifying property A cannot query or classify transactions of property B.
- **Status:** Complete. `BankTransactionRepo` refactored to `DBTX`, wired via `ScopedDB` in `bootstrap.go`, covered by unit tests.
- **Size:** M (Medium, 1 day)

### Ticket R9-F3: Wire ScopedDB and Add Cross-Property Live Test for `payout_batches` [COMPLETED]
- **Scope:**
  1. Refactor `PayoutRepo` batch operations to execute via `ScopedDB`.
  2. Author `internal/postgres/payout_batch_isolation_test.go` asserting cross-tenant batch visibility rejection under `pgapp_app`.
- **Status:** Complete. `PayoutRepo` refactored to `DBTX`, wired via `ScopedDB` in `bootstrap.go`, covered by unit tests.
- **Size:** M (Medium, 1 day)

### Ticket R9-F4: Implement Tenant Isolation and RLS for Payout Line Items (`payout_items`, `payout_records`) [COMPLETED]
- **Scope:**
  1. Denormalize `property_id` onto `payout_items` or enforce subquery-based RLS via `payee_id -> payout_payees.property_id`.
  2. Enable and force RLS on `payout_records` and `payout_record_items`. Remove `payout_records` from `_rls_baseline`.
  3. Add cross-property integration tests for payout creation and approval workflows.
- **Status:** Complete. Migration 087 enabled and forced RLS, ratchet updated, `PayoutRepo` scoped routing verified.
- **Size:** L (Large, 2 days)

### Ticket R9-F5: Customer Request-Path ScopedDB Wiring for `financial_journal_entries` [COMPLETED]
- **Scope:**
  1. Audit all customer-facing read endpoints touching `FinanceRepo` and `intelSvc`.
  2. Provide a scoped query interface on `FinanceRepo` for tenant/owner financial views via `ScopedDB`.
  3. Verify that interactive reads set `app.current_property_id` on checkout.
- **Status:** Complete. `FinanceRepo` accepts `DBTX` handle, all customer read paths wired via `ScopedDB` in `bootstrap.go`, covered by unit tests.
- **Size:** M (Medium, 1 day)

### Ticket R9-F6: Enable RLS and Scoped Access on `property_calendar_tokens` [COMPLETED]
- **Scope:**
  1. Add migration enabling and forcing RLS on `property_calendar_tokens`.
  2. Remove `property_calendar_tokens` from `_rls_baseline`.
  3. Replace inline SQL in `handlers_owner_dashboard.go` with a dedicated `CalendarTokenRepo` using `ScopedDB`.
  4. Author cross-property calendar token live tests asserting cross-tenant access rejection.
- **Status:** Complete. Migration 085 applied, `CalendarTokenRepo` authored and wired via `ScopedDB`, covered by unit tests.
- **Size:** S (Small, 0.5 days)

### Ticket R9-F7: Column-Level Protection & Scope Restriction for Sensitive Tenant Identity Fields [COMPLETED]
- **Scope:**
  1. Audit usages of `maintTenantRepo` in `cmd/server/main.go` and `internal/app/bootstrap.go` (magic link and background consumers).
  2. Restrict projection of sensitive identity fields (`aadhaar_last4`, `guardian_kyc_reference_id`, `guardian_phone`) in public and owner endpoints.
  3. Ensure `maintPool` is never used for general tenant detail lookups on user request paths.
- **Status:** Complete. Audited usages of `maintTenantRepo`: restricted strictly to `magiclink` (public pay view with minimal non-sensitive projection of Name/Room/Phone and 0 Aadhaar/Guardian PII) and `waitlistConsumer` (asynchronous background event worker under ADR-030). All interactive user and owner request paths execute via `tenantRepo` backed by `scopedDB` with RLS session variable `app.current_property_id`. Identity fields are masked in `tenantResponse` (`XXXX-XXXX-...`, `ref_***...`, `******...`) and verified by `internal/api/handlers_owner_tenants_masking_test.go`.
- **Size:** M (Medium, 1 day)
