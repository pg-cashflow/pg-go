# ADR-025: Round 6 Audit Verification, Invariant Hardening, and Dead Schema Field Cleanup

**Status:** Accepted  
**Date:** 2026-10-11  
**Basis:** Round 6 Empirical Verification and Defect Remediation  
**Deciders:** Architecture / Core Engineering  
**Standard:** ASD-STE100 (Simplified Technical English)  
**Method:** Empirical First-Principles Verification (Karpathy Method)  

---

## 1. Context

Codebase verification of the Round 6 upload identified five operational items:

1. **Migration and ADR Numbering Conflict:** The upload provided `064_finance_settings_and_policy_invariants.sql` and `docs/adr/024-finance-policy-invariants-error-parity-and-token-hardening.md`. These files replace previous uncommitted variants with identical numbering. Migration 064 is idempotent and valid. Databases with previous versions recorded must be rebuilt or reconciled.
2. **Duplicate Schema Column and Dead Field:** The `properties` table contains both `payment_mode` and `payment_collection_mode`. Analysis confirmed that the Go application reads and writes only `payment_mode`. The domain struct field `Property.PaymentCollectionMode` is never populated from queries or written by commands.
3. **Parity and Invariant Test Fixtures Used Dead Column:** New integration tests in `live_policy_parity_test.go` and `finance_invariants_db_test.go` inserted test records using `payment_collection_mode` instead of the active `payment_mode` column.
4. **Weakened Policy Invariant Test:** In `service_test.go`, `TestPatchUnifiedSettings` omitted threshold fields (`OwnerApprovalThresholdPaise`, `ReimbursementThresholdPaise`, and `EmergencyBypassEnabled`). The struct initialization defaulted these values to zero. Because zero is accepted by policy validation, the test failed to detect accidental omission of threshold fields.
5. **Modified Client Error Message:** The error message for `domain.ErrInvalidAmount` was changed from `"finance: invalid amount"` to `"finance: amount must be positive"`. The change was not documented in API contracts.

---

## 2. Decisions

### D1. Remove Dead Go Struct Field and Retain Database Column
- Remove `PaymentCollectionMode` from `domain.Property` in `internal/domain/property.go`.
- Retain the database column `payment_collection_mode` in PostgreSQL.
- Rationale: ADR-022 specifies that a database column must not be dropped in the same release cycle that stops its application usage (expand-contract pattern). The column will be dropped in a future migration.

### D2. Point Integration Test Fixtures to Active Column
- Update `internal/finance/live_policy_parity_test.go` to insert `payment_mode = 'manual'`.
- Update `internal/postgres/finance_invariants_db_test.go` to insert `payment_mode = 'manual'`.
- Rationale: Tests must exercise the live column used by production workflows.

### D3. Restore Complete Policy Invariant Test
- Update `TestPatchUnifiedSettings` in `internal/finance/service_test.go` with explicit threshold values:
  - `OwnerApprovalThresholdPaise: 500000`
  - `ReimbursementThresholdPaise: 250000`
  - `EmergencyBypassEnabled: true`
- Add explicit assertions verifying that all threshold fields persist correctly after the patch operation.

### D4. Document Error String Update
- Record the error string update for `domain.ErrInvalidAmount` and `finance.ErrInvalidAmount`:
  - New message: `"finance: amount must be positive"`
  - Previous message: `"finance: invalid amount"`
- Static analysis confirmed that API consumers in this repository match errors using `errors.Is` and error codes rather than exact error string comparison.

### D5. Adopt Migration 064 and ADR-024
- Accept the uploaded `064_finance_settings_and_policy_invariants.sql` and ADR-024 as canonical.
- Document that environments with an older version of Migration 064 must rebuild the test database from migration 001.

---

## 3. Empirical Verification & Evidence

All invariants were verified on a fresh PostgreSQL database with the Go race detector enabled.

### 3.1 Verification Summary

| Target Suite | Test Command | Result | Details |
|---|---|---|---|
| Complete Test Suite | `go test -race ./...` | Pass | 0 failures, 0 skips, 93 live database tests executed |
| Finance Unit & Parity | `go test -v -race ./internal/finance` | Pass | Error identity parity, memory store, and policy invariants pass |
| Invariants Database Test | `go test -v -race ./internal/postgres -run TestLivePostgres_FinanceInvariantsDatabaseConstraints` | Pass | Database check constraints correctly reject invalid inserts |
| Service Settings Patch | `go test -v -race ./internal/finance -run TestPatchUnifiedSettings` | Pass | Full policy thresholds applied and asserted |
| Calendar Token Security | `go test -v -race ./internal/api -run TestVerifyCalendarToken_SecurityEdgeCases` | Pass | 7 security edge cases pass with expired tokens returning 401 |

### 3.2 Defect Mutation Injection Results

Five intentional regressions were injected into scratch copies to verify that tests fail when invariants are violated:

1. **Error Alias Regression:** Reverting `finance.ErrPolicyExceeded` to an independent error instance failed `TestLivePostgres_PolicyExceededErrorIdentity` with error identity mismatch.
2. **Ordering Invariant Regression:** Removing the ordering check `single <= daily <= monthly` failed `TestValidateApprovalPolicy_Invariants`.
3. **Calendar Token Expiry Regression:** Disabling the expiration check failed `TestVerifyCalendarToken_SecurityEdgeCases`.
4. **Fiscal Range Regression:** Allowing day 29 failed `TestValidateFinanceSettings_FiscalRange`.
5. **Database Constraint Drop:** Dropping `chk_approval_policies_ordering` in PostgreSQL failed `TestLivePostgres_FinanceInvariantsDatabaseConstraints`.

---

## 4. Open Items & Future Work

1. **Outcome Assertions:** Expand approximately 35 route smoke tests from HTTP status-code checks to business payload assertions.
2. **Owner-to-Owner Isolation:** Verify tenant-to-tenant and owner-to-owner isolation when multiple owners share property boundaries.
3. **Validate Database Constraints:** Migration 064 applied constraints as `NOT VALID`. Execute `ALTER TABLE ... VALIDATE CONSTRAINT` after correcting historical legacy rows.
4. **Drop Deprecated Column:** Remove `payment_collection_mode` from the `properties` table in a future release.
5. **Durable Payout Audit Table:** Migrate payout export audit lines from log sinks to a durable PostgreSQL audit table.
