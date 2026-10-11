# ADR-027: Coverage Ratchet Restoration, Cashfree Guard Hardening, and Load Workflow Remediation

**Status:** Accepted  
**Date:** 2026-10-11  
**Basis:** Round 8 Defect Remediation and Verification Hardening  
**Deciders:** Architecture / Core Engineering  
**Standard:** ASD-STE100 (Simplified Technical English)  
**Method:** Empirical First-Principles Verification (Karpathy Method)  

---

## 1. Context

Strict verification of the Round 7 upload identified four silent regressions and defects:

1. **Coverage Ratchet Regression:** `scripts/check-coverage.go` was reverted to old baseline floors (75/60/50/45). The `-report-aspirational` flag and its CI invocation were removed. This change weakened test coverage enforcement on critical packages.
2. **Incomplete Production Guard:** The `CASHFREE_PG_BASE_URL` loopback override was blocked only when `APP_ENV` equaled `production`. If `CASHFREE_ENV=production` while `APP_ENV` was unset or `development`, the system accepted a loopback override. This defect exposed production gateway credentials to a loopback redirect.
3. **Load Workflow Missing Gateway Credentials:** `.github/workflows/loadtest.yml` started the server with `CASHFREE_PG_BASE_URL` but omitted `CASHFREE_PG_APP_ID`, `CASHFREE_PG_SECRET_KEY`, and `CASHFREE_PG_WEBHOOK_SECRET`. The checkout endpoint returns HTTP 503 when Cashfree credentials are missing. The load test would fail during order creation rather than reaching the fake gateway.
4. **k6 APT Keyserver Fragility:** The load workflow downloaded k6 via an Ubuntu keyserver. Keyserver network failures frequently break CI builds.
5. **Ticket and Map Inconsistency:** `docs/wayfinder/tickets/track_r07_gate07_migration_tests.md` displayed status `Open`, whereas `docs/wayfinder/map.md` recorded Gate 07 as `Closed`.

---

## 2. Decisions

### D1. Restore Coverage Ratchet and Aspirational Gap Reporting

- Restore the coverage ratchet in `scripts/check-coverage.go`. Set floors to measured statement coverage minus 1.5 to 2.0 points:
  - `internal/finance`: 76.0% (measured: 77.4%–77.6%)
  - `internal/auth`: 64.0% (measured: 65.6%)
  - `internal/payment`: 51.5% (measured: 53.4%)
  - `internal/api`: 62.0% (measured: 62.9%–64.0%)
  - Secondary packages maintain their established floors: `jobs` (60.0%), `gamification` (35.0%), `postgres` (35.0%), `notification` (14.0%), `tenant` (10.0%).
- Add the `-report-aspirational` CLI flag to `scripts/check-coverage.go`.
- Print the aspirational gap report for finance (85.0%), payment (85.0%), auth (80.0%), and api (75.0%) without failing the build.
- Update `.github/workflows/ci.yml` to execute:
  `go run ./scripts/check-coverage.go -profile=coverage.out -require-all -report-aspirational`
- Mark ADR-026 as superseded in part regarding coverage floors.

### D2. Harden the Production Override Guard

- Modify `internal/config/config.go` to reject `CASHFREE_PG_BASE_URL` if either `APP_ENV` or `CASHFREE_ENV` equals `production`.
- Use `strings.EqualFold` to perform case-insensitive evaluation (`production`, `Production`, `PRODUCTION`).
- Add a 6-case table-driven test `TestLoad_CashfreePGBaseURL_ProductionGuard` in `internal/config/config_test.go` covering all valid and invalid environment variable combinations. Assert fail-closed behavior specifically on the production override error string.

### D3. Remediate Load Testing Workflow

- Supply fake gateway credentials in `.github/workflows/loadtest.yml`:
  - `CASHFREE_PG_APP_ID: fake_app_id_for_loadtest`
  - `CASHFREE_PG_SECRET_KEY: fake_secret_key_for_loadtest`
  - `CASHFREE_PG_WEBHOOK_SECRET: fake_webhook_secret_for_loadtest`
  This configuration enables the Cashfree client and allows checkout requests to target `http://127.0.0.1:8081`.
- Replace the APT keyserver k6 installation with `grafana/setup-k6-action@v1`.
- Install `postgresql-client` in a dedicated APT step because `scripts/loadtest/run.sh` invokes `psql`.

### D4. Reconcile Gate 07 Status

- Set status in `docs/wayfinder/tickets/track_r07_gate07_migration_tests.md` to `Closed`. This status matches `docs/wayfinder/map.md` and the passing `TestGate07_*` suite.

---

## 3. Empirical Verification and Evidence

### 3.1 Production Guard Verification and Revert Checks

- Table test `TestLoad_CashfreePGBaseURL_ProductionGuard` executes 6 subtests in `internal/config/config_test.go`:
  1. `app_env_production_cf_sandbox`: PASS (error rejected)
  2. `app_env_production_cf_unset`: PASS (error rejected)
  3. `app_env_development_cf_production`: PASS (error rejected)
  4. `app_env_unset_cf_production_case_insensitive`: PASS (error rejected)
  5. `app_env_development_cf_sandbox_allowed`: PASS (override accepted)
  6. `app_env_test_cf_unset_allowed`: PASS (override accepted)
- **Revert Check 1 (`CASHFREE_ENV` guard):** Removing the `CASHFREE_ENV` check causes exactly subtests 3 and 4 to fail with `expected error when overriding base URL in production`.
- **Revert Check 2 (Loopback guard):** Disabling the loopback verification fails the remote-host and private-address tests in `TestLoad_CashfreePGBaseURL`.

### 3.2 Coverage Gate Verification and Revert Checks

- `go run scripts/check-coverage.go -profile=... -require-all -report-aspirational` exits 0.
- Reported gaps:
  - finance: 7.6 points (85.0% target vs 77.4% actual)
  - payment: 31.6 points (85.0% target vs 53.4% actual)
  - auth: 14.4 points (80.0% target vs 65.6% actual)
  - api: 12.1 points (75.0% target vs 62.9% actual)
- **Revert Check 3 (Ratcheted floor):** Raising the finance floor to 78.0% fails Gate 02 (`Actual: 77.4% < Floor: 78.0%`).

### 3.3 Workflow and Code Quality

- `.github/workflows/loadtest.yml` validated as syntax-clean YAML.
- `gofmt -l .` reports zero files requiring formatting.
- `go vet ./internal/... ./cmd/...` exits 0 with zero warnings.

---

## 4. Status of Open Items

- Item 1 (Sandbox repository secrets): Pending operator credential provisioning.
- Item 2 (Manual load test trigger): Ready for execution via GitHub Actions.
- Item 3 (Package coverage gaps): Tracked via `-report-aspirational`.
- Item 4 (Staging environment): Blocked pending external infrastructure.
- Item 5 (k6 official action): **Resolved** (migrated to `grafana/setup-k6-action@v1`).
