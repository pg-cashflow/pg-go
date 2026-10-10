# ADR-023: Route Authorization Parity, Fail-Closed Observability, and Finance Invariants

**Status:** Accepted  
**Date:** 2026-10-10  
**Basis:** Audit Review & Verification of System Hardening  
**Deciders:** Architecture / Core Engineering  
**Standard:** ASD-STE100 (Simplified Technical English)  
**Method:** Empirical First-Principles Verification (Karpathy Method)  

---

## 1. Context

System audit and adversarial testing identified seven security and operational risks:
1. **Public Route Authorization Loophole:** The route-role authorization matrix classified public routes using path prefixes (`/api/auth/`, `/webhooks/`, `/api/public/`). An unreviewed route added under these prefixes became public without verification.
2. **Metrics Fail-Open Default:** The `/metrics` endpoint allowed unauthenticated access when `METRICS_TOKEN` was unset in non-production environments. This exposed internal operational data by default.
3. **Weak Migrator Test Assertion:** `cmd/migrate/main_test.go` asserted the missing `DATABASE_URL` error message using `t.Logf` instead of `t.Fatalf`. The test passed even when the failure message changed.
4. **Billing Clock Inconsistency:** Billing service used `time.Now()` directly in its default configuration rather than the shared `internal/timeutil` clock abstraction.
5. **Coverage Floor Gaps:** Notification and tenant packages lacked explicit regression floors in `scripts/check-coverage.go`.
6. **Finance Settings Destruction via Replace Semantics:** `PATCH /api/owner/finance/settings` and `PATCH /api/owner/finance/policies` replaced the entire settings struct. Omitted fields reset to zero. In the policy evaluator, zero values blocked all manager expenditures and silently disabled emergency bypass. Out-of-range thresholds were accepted without validation.
7. **Unverified Same-Property Tenant Isolation:** Authorization tests verified cross-property boundaries, but same-property tenant-to-tenant data isolation remained unverified.

---

## 2. Decisions

### D1. Exact Public Route Allowlist in Authorization Matrix
- Replace prefix-based matching with an exact allowlist map (`exactPublicPaths`) in `internal/api/route_role_matrix_test.go`.
- Validate that all public paths match registered router endpoints. Fail the test if an entry in `exactPublicPaths` is stale or missing from the router.
- Enforce that any unclassified route fails the matrix.

### D2. Fail-Closed Metrics Handler
- Require `METRICS_TOKEN` for `/metrics` and `/api/metrics` in all environments by default.
- Reject requests with HTTP 401 `apierr.CodeAuthUnauthorized` when no valid token is provided.
- Allow unauthenticated access only when `METRICS_ALLOW_OPEN=true` is set explicitly.

### D3. Strict Migrator Output Assertion
- In `cmd/migrate/main_test.go`, assert that command failure output contains `DATABASE_URL is required` using `t.Fatalf`.
- Ensure tests fail immediately if the error message or exit code regresses.

### D4. Standardized Billing Clock
- Wire `timeutil.System.Now` as the default clock source in `internal/billing/service.go`.
- Maintain test clock injection capability for leap year and month-end boundary testing.

### D5. Coverage Regression Floors
- Add `internal/notification` with floor 14.0% to `scripts/check-coverage.go`.
- Add `internal/tenant` with floor 10.0% to `scripts/check-coverage.go`.

### D6. Finance Configuration Merge and Validation
- Define pointer-valued patch models `FinanceSettingsPatch` and `ApprovalPolicyPatch` in `internal/domain/finance.go`.
- In `internal/finance/service.go`, implement `MergeFinanceConfig` to load current settings and overwrite only fields provided in the patch payload.
- Enforce validation rules in `ValidateApprovalPolicy` and `ValidateFinanceSettings`:
  - Limits must be non-negative.
  - Single transaction limit must not exceed daily limit: `single_limit <= daily_limit`.
  - Daily limit must not exceed monthly limit: `daily_limit <= monthly_limit`.
  - Fiscal year start day must fall in range 1 to 28.
- Register new error code `CodeFinanceInvalidSettings = "finance.invalidSettings"` mapped to HTTP 400 Bad Request in `internal/apierr` and `CONTRACT.md`.

### D7. Same-Property Tenant Isolation Verification
- Create `internal/api/tenant_isolation_test.go` with two tenants belonging to the same property.
- Verify Tenant A cannot view or mutate Tenant B's dues, QR codes, checkout intents, dues reports, or payment records.
- Verify Tenant B cannot access Tenant A's records.

### D8. HTTP Route Outcome Verification
- Create `internal/api/routes_outcome_test.go` covering endpoints that previously lacked static path literals in tests.
- Exercise request and response pipelines for authentication, invite rotation, finance summaries, manager endpoints, and tenant writes.

---

## 3. Empirical Verification & Evidence

All changes were verified using first-principles execution and defect injection:

| Target Component | Test Command / Suite | Empirical Result | Defect Injection Test |
|---|---|---|---|
| Route Matrix Allowlist | `go test ./internal/api -run TestRouteRoleAuthorizationMatrix` | Pass (205 routes, 1095 checks) | Injected unreviewed route `/api/auth/unreviewed`: test failed immediately. |
| Metrics Fail-Closed | `go test ./internal/api -run TestMetricsEndpoint` | Pass (3 subtests) | Removing `METRICS_ALLOW_OPEN`: unauthenticated requests rejected with 401. |
| Migrator Fail-Closed | `go test ./cmd/migrate -run TestMigrate_FailClosedWithoutDB` | Pass (exit non-zero + message asserted) | Changed expected message to incorrect string: test failed with `t.Fatalf`. |
| Billing Clock | `go test ./internal/billing/...` | Pass (10 packages tested) | Default clock verified through `timeutil.System.Now`. |
| Coverage Floors | `go run scripts/check-coverage.go -require-all` | Pass | Tested packages meet or exceed assigned regression floors. |
| Finance Partial Merge | `go test ./internal/finance -run TestMergeFinanceConfig` | Pass (11 subtests) | Reverting to replace semantics failed partial update test. Removing validation failed range tests. |
| Error Code Parity | `go test ./internal/apierr ./internal/api -run "TestContract|TestErrorCodes|TestAllCodes"` | Pass (parity between JSON, constants, and CONTRACT.md) | Omitted code from table: table parity test failed. |
| Same-Property Tenant Isolation | `go test -count=1 ./internal/api -run TestSamePropertyTenantIsolation` | Pass (7 subtests) | Injected bypass removing `due.TenantID != t.ID`: QR test failed with 200 instead of 404. |
| Route Outcome Verification | `go test -count=1 ./internal/api -run TestUnreferencedRoutesOutcome` | Pass (19 subtests) | Verified 19 endpoint outcome paths against HTTP requests. |

---

## 4. Open Items & Future Work

1. **Durable Payout Audit Records:** Payout export audit markers currently write to the structured logger. Persisting durable ledger events requires new domain event schemas and database migration.
2. **Database Check Constraints for Finance Limits:** Service invariants validate approval limits in memory. Direct SQL writers could bypass this check without database constraints.
3. **Aspirational Coverage Targets (85/85/80/75):** Reaching target levels requires adding branch coverage across edge cases in remaining modules.
4. **Staging Gateway and Load Testing:** K6 load testing and Cashfree sandbox gateway validation require dedicated staging infrastructure.
