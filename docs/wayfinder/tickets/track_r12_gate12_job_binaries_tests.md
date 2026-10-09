# Ticket 28: Track R.12 — Gate 12: Background Job CLI Flags, Signal Handling & Exit Codes

- **Type**: `wayfinder:task`
- **Status**: Open
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Specification**: [Test Readiness Specification](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/TEST_READINESS_SPECIFICATION.md)
- **Dependencies**: None
- **Tier**: PR Gate (< 10 min)

## Objective

Add automated tests for all background cron and operational binaries in `cmd/`.
Assert that every binary parses command-line flags, responds to termination signals, and emits standard exit codes.
Currently, the 10+ binary entry points in `cmd/` have zero direct automated tests.

## Target Binaries

- `cmd/server`
- `cmd/billing-cycle`
- `cmd/reminder`
- `cmd/cashfree-poll`
- `cmd/digilocker-reconcile`
- `cmd/kyc-expiry`
- `cmd/financial-summary`
- `cmd/kpi-snapshot`
- `cmd/gamification-cycle`
- `cmd/search-reindex`
- `cmd/migrate`

## Karpathy Test Discipline

1. **Look at data first:**
   Inspect `main.go` for each binary in `cmd/`.
   Identify flags, environment variables, and exit code branches.

2. **Check state at initialization:**
   Verify whether business logic is decoupled into `internal/jobs` functions.

3. **Overfit one example:**
   Select `cmd/billing-cycle`.
   Test execution with `--help` -> assert exit code 0.
   Test execution with invalid flag `--unknown` -> assert exit code 2.
   Test execution with missing `DATABASE_URL` -> assert non-zero exit code.

4. **Compare with dumb baseline:**
   Verify each binary implements standard POSIX exit conventions:
   - 0: Successful completion or help output.
   - 1: Runtime/configuration error.
   - 2: Command-line syntax error.

5. **Fix seeds:**
   Deterministic argument lists.

6. **Change one thing at a time:**
   Test CLI flags and exit codes first.
   Test signal termination (`SIGINT`, `SIGTERM`) second.
   Test job execution with mock database third.

## STE-100 Implementation Steps

1. Create test file `internal/jobs/jobs_cli_test.go` or `cmd/jobs_test.go`.
2. Write a table-driven test covering all 11 binary entrypoints.
3. For each binary, compile a temporary test binary using `go build`.
4. Test Case 1 (Help Flag):
   - Execute binary with `--help`.
   - Assert exit code 0.
   - Assert standard usage documentation is printed to stdout.
5. Test Case 2 (Invalid Flag):
   - Execute binary with `--invalid-flag-xyz`.
   - Assert non-zero exit code.
6. Test Case 3 (Missing Required Config):
   - Execute binary with empty environment.
   - Assert binary fails fast and prints a clear configuration error.
7. Test Case 4 (Graceful Shutdown):
   - Start long-running binary (e.g. `cmd/server`).
   - Send `syscall.SIGTERM`.
   - Assert binary exits cleanly within timeout.

## Acceptance Criteria

- Every binary in `cmd/` has an automated test validating flags and startup.
- All binaries exit with standard POSIX status codes.
- All binaries log missing environment variables clearly.
- Tests execute in CI within 30 seconds.
