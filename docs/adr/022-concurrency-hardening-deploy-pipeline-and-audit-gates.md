# ADR-022: Concurrency Hardening, Deploy Pipeline, and Audit Gates

**Status:** Accepted  
**Date:** 2026-10-10  
**Basis:** Audit Review & Verification of Concurrency Guards  
**Deciders:** Architecture / Core Engineering  
**Standard:** ASD-STE100 (Simplified Technical English)  
**Method:** Empirical First-Principles Verification (Karpathy Method)  

---

## 1. Context

Code integrity audit and adversarial analysis identified operational and security risks:
1. **Concurrency Atomicity Gap:** OTP and payment token invalidation required verification against race conditions. Previous mock tests hid SQL predicate regressions.
2. **Payout Export Observability:** Payout exports lacked audit trail logging for verified file generation.
3. **Deployment Coupling & Rollback Fragility:** The deployment workflow ran migrations without test database verification and lacked HTTP health checks for binary rollback.
4. **Clock Injection (Gate 13):** Services used global `time.Now()` directly. This prevented deterministic simulation of leap years, month-end date clamping, and IST boundaries.
5. **Zero-Test Package Coverage:** Critical packages (`internal/requestscope`, `internal/apierr`, `internal/events`, `internal/web`, and all `cmd/*` binaries) had no dedicated tests.
6. **Coverage Gate Silos:** Coverage gates did not enforce `-require-all`, permitting untested packages to pass CI silently.

---

## 2. Decisions

### D1. Atomic Predicate Concurrency Guards
- In `OTPRepo.MarkUsed` and `TokenRepo.MarkUsed`, execute conditional update:
  ```sql
  UPDATE otp_requests SET used=TRUE WHERE id=$1 AND used=FALSE;
  UPDATE payment_tokens SET used=TRUE WHERE id=$1 AND used=FALSE;
  ```
- If rows affected equals 0, return `ErrOTPAlreadyUsed` or `ErrTokenAlreadyUsed`.
- Enforce atomicity with live PostgreSQL parallel concurrency tests in `otp_repo_live_test.go`. Ten concurrent goroutines race to mark the same record. The test asserts exactly one winner and nine errors. Fixture setup uses real properties, tenants, and dues rows with fatal setup assertions.

### D2. Payout Export Audit Logging
- In `OwnerExportPayoutBatch`, write a structured audit log entry when export succeeds:
  ```go
  slog.Info("payout batch exported",
      "audit", "payout_export",
      "actor_id", actorID,
      "property_id", pid,
      "batch_id", batch.ID,
      "batch_number", batch.BatchNumber,
      "row_count", len(items),
      "checksum", checksumVal,
  )
  ```
- Step-up re-authentication is not required on the GET export endpoint. The CSV export contains no beneficiary bank account numbers and is restricted to approved batches for the property owner.
- Future policy trigger: Step-up re-authentication will be enforced if staff delegation is implemented or if unmasked bank account numbers are added to export files.

### D3. Three-Stage Deploy Pipeline with Atomic Rollback
- Split `.github/workflows/deploy.yml` into three sequential jobs:
  1. `verify`: Runs database migrations and unit/integration tests with `REQUIRE_DB=1` on a PostgreSQL service container.
  2. `migrate`: Runs on a GitHub-hosted runner (`ubuntu-latest`) against `NEON_DATABASE_URL` with production environment approval. This decouples schema updates from host server availability.
  3. `deploy`: Compiles production binary, swaps `/opt/pg-app/server`, restarts service on `[self-hosted, pg-app-server]`, and polls `/healthz`. If `/healthz` fails, it rolls back to `server.bak`.
- Migrations must follow the expand-only rule. Do not drop or rename columns in the same release as binary updates.

### D4. Time and Calendar Clock Abstraction (`internal/timeutil`)
- Provide `timeutil.Clock` interface with `Now() time.Time`.
- Provide `SystemClock` for UTC system time and `FixedClock` for deterministic tests.
- Wire clock into `finance.Service`, defaulting `Now` to `timeutil.System.Now`.

### D5. Package Test Pyramid Completion
- Add tests for all unverified packages:
  - `internal/requestscope`: Scope round trip, absent context, nil UUID rejection, context isolation.
  - `internal/apierr`: Error code uniqueness, pattern validation, JSON envelope serialization, `Abort` mechanics.
  - `internal/events`: Fail-closed persist semantics, asynchronous subscriber broadcast, panic recovery.
  - `internal/web`: Embedded index existence, client-side route fallback, directory traversal protection.
  - `cmd/*`: Subprocess execution verifying non-zero exit and absence of panics for twelve fail-closed binaries, and no-op exit for `search-reindex`.

### D6. CI Gate Hardening
- Enforce `-require-all` in `check-coverage.go`.
- Add regression coverage floors for `jobs` (60.0%), `gamification` (35.0%), and `postgres` (35.0%).
- Run `ledger_controls_test.sql` against migrated test database in CI.
- Run 30-second fuzz smoke tests per pull request across all five fuzz targets.

---

## 3. Consequences

### Positive
- Race conditions during OTP validation and magic-link usage are blocked at the database engine level.
- Payout file downloads produce a structured audit log entry in the log sink.
- Deployments fail safely with automatic binary rollback if `/healthz` does not respond.
- Calendar and leap-year invariants can be verified without system clock changes.
- CI fails if any tracked package loses test coverage or skips tests.

### Negative / Trade-Offs
- Multi-step migrations are required when dropping or renaming database columns.
- Payout export audit lines are written to the application log sink, not to the immutable database ledger.

---

## 4. Open Items

1. **Durable Ledger Audit for Payout Exports:** Emitting durable payout events requires registering payout export event types in the domain events catalog.
2. **Aspirational Coverage Targets:** Gate 02 aspirational targets (85/85/80/75) remain target goals while regression floors enforce build safety.
3. **Billing Clock Integration:** `internal/billing` still calls `time.Now()` directly and requires migration to `timeutil.Clock`.
