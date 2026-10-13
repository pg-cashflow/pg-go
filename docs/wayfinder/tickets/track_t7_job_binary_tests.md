# Ticket T7: Job Binary Tests (G8)

- **Track:** T7
- **Source:** `docs/wayfinder/SPEC_TEST_GAPS_AND_TICKETS.md`
- **Status:** Closed
- **Resolved By:** `internal/jobs/jobs_cli_test.go`, `cmd/search-reindex/main.go`, `cmd/search-reindex/main_test.go`

## Acceptance Criteria
- [x] For each `cmd/*` binary:
  - `-help` exits 0.
  - An invalid flag exits non-zero.
- [x] Run against the test database exits 0 and is safe to run twice (idempotency verified for cron binaries).
- [x] Ensure all 13 `cmd/*` binaries (`api`, `billing-cycle`, `daily-rollup`, `reminder`, `cashfree-poll`, `digilocker-reconcile`, `financial-summary`, `gamification-cycle`, `kpi-snapshot`, `kyc-expiry`, `migrate`, `search-reindex`, `sandbox-smoke`) are verified in `internal/jobs`.
