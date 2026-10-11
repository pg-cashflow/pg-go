# ADR-024: Finance Invariant Enforcement, Error Identity Parity, and Security Hardening

**Status:** Accepted  
**Date:** 2026-10-11  
**Basis:** Audit Verification and Empirical System Hardening  
**Deciders:** Architecture / Core Engineering  
**Standard:** ASD-STE100 (Simplified Technical English)  
**Method:** Empirical First-Principles Verification (Karpathy Method)  

---

## 1. Context

Code verification and empirical testing identified discrepancies between documentation, database constraints, and service behavior:

1. **Production Error Identity Mismatch:** The PostgreSQL repository returned `domain.ErrPolicyExceeded`. The HTTP error mapper evaluated `finance.ErrPolicyExceeded`. Because these were distinct error instances, the check failed and the HTTP layer returned a generic 500 Internal Server Error instead of 400 Bad Request. The same discrepancy affected `ErrInvalidAmount`.
2. **Duplicated Spend Policy Evaluator:** `domain.EvaluateManagerSpend` and `finance.EvaluateManagerSpend` contained separate implementations of identical logic. Changes in one risked silent divergence from the other.
3. **Missing Ordering Invariant Enforcement:** ADR-023 specified `single_limit <= daily_limit <= monthly_limit`. The service validator checked upper ceilings but did not check ordering.
4. **Permissive Fiscal Month Start Day Range:** `ValidateFinanceSettings` permitted days 1 to 31. Days 29 to 31 do not exist in every calendar month (such as February). This created a risk of invalid period calculations.
5. **Ceiling Value Comment Discrepancy:** The literal constant `1_000_000_000_00` equals 10^11 paise (100 crore INR). The code comment stated "10 crore paise" (10^10).
6. **Unused Whole-Object Mutation Trap:** `Service.PatchSettings` used full replace semantics. It had zero production callers and remained only in a test.
7. **Missing Database Invariant Constraints:** Approval limits and fiscal settings were validated only in memory. Writers outside the service could insert invalid records directly into PostgreSQL.
8. **Untested Calendar Token Security Properties:** Calendar feed tokens enforce property isolation and expiration, but test coverage lacked targeted edge cases for expired tokens, altered payloads, and forged signatures.
9. **Unformatted Database Files:** Four repository files in `internal/postgres/` had formatting drift from comment normalization.

---

## 2. Decisions

### D1. Unified Error Identity and Delegation to Domain
- In `internal/domain/policy.go`, standardize `ErrPolicyExceeded` and `ErrInvalidAmount`.
- In `internal/finance/errors.go`, alias `ErrPolicyExceeded = domain.ErrPolicyExceeded` and `ErrInvalidAmount = domain.ErrInvalidAmount`.
- In `internal/finance/policy.go`, remove duplicated implementations and delegate all spend checks, defaults, and helpers directly to `domain`.
- `SpendCheck` in `finance` is an alias of `domain.SpendCheck`.

### D2. Enforce Strict Ordering Invariant in Approval Policy
- In `internal/finance/service.go`, enforce `p.SingleExpenseLimitPaise <= p.ManagerDailyLimitPaise` and `p.ManagerDailyLimitPaise <= p.ManagerMonthlyLimitPaise` in `ValidateApprovalPolicy`.
- Return `ErrInvalidSettings` mapped to HTTP 400 Bad Request when the invariant is violated.

### D3. Restrict Fiscal Start Day to Valid Monthly Range (1 to 28)
- In `internal/finance/service.go`, enforce `s.FiscalMonthStartDay >= 1 && s.FiscalMonthStartDay <= 28` in `ValidateFinanceSettings`.
- Reject days 29, 30, and 31 to prevent irregular month-boundary calculations.

### D4. Correct Ceiling Comment
- Correct the comment on `MaxExpenseLimitPaise = 1_000_000_000_00` to `10^11 paise (100 crore INR)`.

### D5. Remove Deprecated Whole-Object Mutation Method
- Delete `Service.PatchSettings` from `internal/finance/service.go`.
- Remove corresponding invocation from `internal/finance/coverage_boost_test.go`.
- Direct callers to `MergeFinanceConfig` and `PatchUnifiedSettings` with complete, valid structures.

### D6. Add Database Check Constraints as NOT VALID (Migration 064)
- Create `migrations/064_finance_settings_and_policy_invariants.sql`.
- Add constraints under ADR-022 expand phase with `NOT VALID` to prevent migration lockups or failures on legacy rows:
  - `chk_property_finance_settings_fiscal_day`: `fiscal_month_start_day BETWEEN 1 AND 28`
  - `chk_property_finance_settings_tdr_bps`: `tdr_effective_bps BETWEEN 0 AND 10000`
  - `chk_approval_policies_ordering`: `single_expense_limit_paise <= manager_daily_limit_paise AND manager_daily_limit_paise <= manager_monthly_limit_paise`
  - `chk_approval_policies_non_negative`: all limits `>= 0`
  - `chk_approval_policies_max_ceiling`: all limits `<= 100000000000`

### D7. Comprehensive Calendar Token Security Tests
- In `internal/api/handlers_owner_dashboard_test.go`, add `TestVerifyCalendarToken_SecurityEdgeCases`:
  - Verify rejection of expired tokens.
  - Verify rejection of cross-property tampered tokens.
  - Verify rejection of forged signatures.
  - Verify rejection of tokens signed with different secrets.
  - Verify rejection of malformed token strings.
  - Verify HTTP 401 Unauthorized returned by `/api/owner/calendar.ics` for expired tokens.

### D8. Preserve Documented KYC Webhook Behavior
- Retain HTTP 200 response on invalid HMAC signature for the KYC webhook (`BadHMAC_Returns200NotError`).
- Rationale: Payment gateways retry on non-2xx status codes. When secrets rotate, returning 200 stops gateway retry loops while internal security telemetry logs the signature failure. Payment webhooks continue to return HTTP 401.

### D9. Code Formatting
- Format `ledger_outbox_repo.go`, `search_repo.go`, `search_v2_perf_gate_test.go`, and `settlement_decision.go` using `gofmt`.

---

## 3. Empirical Verification & Evidence

All changes were verified using tests and live database executions:

| Target Component | Test Command / Suite | Empirical Result | Defect Injection Verification |
|---|---|---|---|
| Formatting | `gofmt -l .` | Pass (zero drift) | Verified all 4 previously drifted postgres files formatted. |
| Error Identity Parity | `go test -v ./internal/finance -run TestLivePostgres_PolicyExceededErrorIdentity` | Pass (0.39s) | Live PostgreSQL test proves manager exceeding daily cap returns error matching `finance.ErrPolicyExceeded`. |
| Fiscal Range & Invariants | `go test -v ./internal/finance -run TestValidateFinanceSettings_FiscalRange` | Pass (13 subtests) | Days 1 to 28 accepted; days 0, -1, 29, 30, 31, and 32 rejected with `ErrInvalidSettings`. |
| Ordering Invariant | `go test -v ./internal/finance -run TestValidateApprovalPolicy_Invariants` | Pass (15 subtests) | Ordering `single <= daily <= monthly` enforced; negative values and ceiling breaches rejected. |
| Error Aliasing & Delegation | `go test -v ./internal/finance -run TestErrorIdentityAndSpendDelegation` | Pass | Errors match across domain and finance; spend evaluation delegates cleanly. |
| Calendar Token Security | `go test -v ./internal/api -run TestVerifyCalendarToken_SecurityEdgeCases` | Pass | Expired tokens, forged signatures, cross-property tampering, and wrong secrets rejected. |
| Database Constraints | `go test -v ./internal/postgres -run TestLivePostgres_FinanceInvariantsDatabaseConstraints` | Pass (1.31s) | PostgreSQL rejects inserts violating fiscal range (day 29), TDR, ordering, negative limits, and ceiling. |
| Migration Continuity | `go test -v ./internal/postgres -run TestGate07_MigrationSequenceContinuity` | Pass | Migration 064 registered with continuous numbering. |

---

## 4. Open Items & Future Work

1. **Validation of Legacy Database Rows:** Migration 064 added constraints as `NOT VALID`. Run `ALTER TABLE ... VALIDATE CONSTRAINT ...` after verifying and updating existing database rows.
2. **Business Outcome Assertions for Smoke Tests:** Expand route outcome tests from status-code assertions to complete payload structure assertions.
3. **Durable Payout Audit Records:** Persist payout export audit records in dedicated ledger tables rather than log sinks alone.
4. **Gate 02 Targets (85/85/80/75):** Increase statement and branch coverage to reach target metrics.
5. **Staging Environment Gates:** Execute k6 load tests and Cashfree sandbox validation in a dedicated staging environment.
