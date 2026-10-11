# ADR-026: Hermetic Load-Test URL Override, Sandbox Smoke Workflow, and Status-Label Reconciliation

**Status:** Accepted  
**Date:** 2026-10-11  
**Basis:** Round 7 Empirical Verification and Defect Remediation  
**Deciders:** Architecture / Core Engineering  
**Standard:** ASD-STE100 (Simplified Technical English)  
**Method:** Empirical First-Principles Verification (Karpathy Method)  

---

## 1. Context

Round 7 audit identified four operational items requiring decisions:

1. **Hard-coded Cashfree base URL.** `internal/cashfree/client.go` exposes an unexported `baseOverride` field usable only from within the package. The load test in `scripts/loadtest/run.sh` starts a local fake gateway but the server's `cmd/server/main.go` never directs the Cashfree client at it. Every `POST /orders` call during a load run would go to `https://sandbox.cashfree.com/pg`, breaking hermeticity and exposing k6 test runs to third-party flakiness.

2. **No GitHub Actions workflow for Gate 09 (load test) or Gate 11 (sandbox smoke).** Both tickets were marked "Resolved" in the ticket file and "Resolved" in map.md, but no workflow file existed and no schedule was active. The k6 scripts and fake gateway existed locally only.

3. **Incorrect status labels.** Gate 02 was marked "Closed" in map.md while CI enforces regression floors (75/50/60/45), not the aspirational targets (85/85/80/75). Track Q was marked "Resolved" while no staging environment exists. Gate 09 was marked "Resolved" but Gate 10 fuzz file paths cited in the map did not include package directories.

4. **KYC webhook 200 on bad signature.** This is a deliberate design decision from a prior round. No change is made here. It is recorded for completeness.

---

## 2. Decisions

### D1. Expose `CASHFREE_PG_BASE_URL` as a Config Field

- Add `CashfreePGBaseURL string` to `internal/config/config.go`, populated from `CASHFREE_PG_BASE_URL`.
- Validation in `Load()`:
  - Prohibited in production (`APP_ENV=production`).
  - Scheme must be `http` (not `https`).
  - Host must be a loopback address: `localhost`, `127.x.x.x`, or `::1`. Private RFC-1918 addresses are rejected.
- Rationale: The restriction to loopback hosts prevents accidental mis-routing to internal network services while allowing a local fake gateway. Production prohibition prevents accidental override in live deployments.

### D2. Expose `WithBaseURL` on `cashfree.Client`

- Add exported `WithBaseURL(override string) *Client` method to `internal/cashfree/client.go`.
- The method sets `baseOverride` and returns the receiver for chaining.
- `cmd/server/main.go` calls `cfClient.WithBaseURL(cfg.CashfreePGBaseURL)` with a warning log when the override is non-empty.
- Rationale: The field was already present and used by tests via direct struct access. Exporting a method gives the server wiring a stable surface that does not require white-box access.

### D3. Write Gate 11 Sandbox Smoke Workflow

- Create `.github/workflows/sandbox-smoke.yml`.
- The workflow is `workflow_dispatch` only. Schedule is commented out.
- A `Verify required Cashfree secrets are present` step runs before any live call. If `CASHFREE_PG_APP_ID`, `CASHFREE_PG_SECRET_KEY`, or `CASHFREE_PG_WEBHOOK_SECRET` is missing, the step exits 1 immediately. The smoke binary itself also exits 1 with no credentials.
- The schedule will be enabled after one successful manual run and after the three secrets are stored as repository secrets.
- Rationale: A weekly schedule that fires before secrets are configured would alert developers every Sunday with a guaranteed failure. The commented-out pattern follows the repository convention in `backup.yml`.

### D4. Write Gate 09 Load Test Workflow

- Create `.github/workflows/loadtest.yml`.
- The workflow accepts `include_soak` and `soak_duration` inputs via `workflow_dispatch`.
- The server is started with `CASHFREE_PG_BASE_URL=http://127.0.0.1:8081`, directing all Cashfree order creation to the fake gateway, not the live sandbox.
- `DATABASE_MAX_CONNS=25` matches the production PgBouncer pool size specified in Ticket 25.
- Schedule is commented out. It will be enabled after a successful manual run confirms k6 installs correctly on the GitHub-hosted runner.
- Rationale: k6 installation from the Grafana APT repository has not been exercised in this environment. Enabling the nightly schedule before a manual run passes would produce nightly failures.

### D5. Correct Status Labels

| Item | Old Label | New Label | Evidence |
|---|---|---|---|
| Gate 02 / Ticket 18 | Closed (map), Open (ticket) | Open (both) | CI runs `-require-all`, not `-strict`. Enforced floors are 75/50/60/45. Current measured: finance 77.6%, auth 65.6%, payment 53.4%, api 64.0%. |
| Gate 09 / Ticket 25 | Resolved | In progress | Workflow written; no run executed. |
| Gate 11 / Ticket 27 | Open | In progress | Workflow written; no run executed. |
| Gate 10 / Ticket 26 | Closed (correct) | Closed; fuzz paths corrected | Map cited bare filenames. Actual paths: `internal/domain/utr_fuzz_test.go`, `internal/cashfree/webhook_fuzz_test.go`, `internal/auth/phone_fuzz_test.go`, `internal/csv/csv_fuzz_test.go`, `internal/aadhaar/qr_fuzz_test.go`. |
| Track Q / Ticket 16 | Resolved | Blocked | No staging server or database provisioned. Binary build and local migration verified. |

---

## 3. Empirical Verification and Evidence

All changes were made to the live `div_dev` branch. No scratch copies were used.

### 3.1 `CASHFREE_PG_BASE_URL` Validation

| Test Case | Rule Verified | Result |
|---|---|---|
| Unset variable | Empty string, `Load()` succeeds | PASS |
| `APP_ENV=production` with override set | Production refusal | PASS |
| `https://localhost:8081` | Scheme must be `http` | PASS |
| `http://api.cashfree.com/pg` | Remote host rejected | PASS |
| `http://192.168.1.100:8081` | Private non-loopback IP rejected | PASS |
| `http://localhost:8081` | Valid loopback by name | PASS |
| `http://127.0.0.1:8081` | Valid loopback by IP | PASS |

Test function: `TestLoad_CashfreePGBaseURL` in `internal/config/config_test.go`.

### 3.2 `WithBaseURL` Client Routing

`TestClient_WithBaseURL` in `internal/cashfree/client_test.go` starts a `httptest.Server`, calls `WithBaseURL(srv.URL)`, and confirms:
- `c.endpoint()` returns the override URL.
- `CreateUPIOrder` sends `POST /orders` to the test server, not to `sandbox.cashfree.com`.

### 3.3 `go vet ./...`

Exit 0 on the full tree after all changes. No new vet warnings.

### 3.4 Config Package Tests

All 14 tests in `internal/config` pass. All 29 tests in `internal/cashfree` pass.

### 3.5 Workflow Syntax

Both workflow files are valid YAML. `bash -n scripts/loadtest/run.sh` exits 0.

---

## 4. Not Verified

- **No workflow has run on GitHub Actions.** Behaviour on a hosted runner with k6 APT install and PostgreSQL service is unknown until the first manual run.
- **No live Cashfree sandbox call was made.** The sandbox is not reachable from this environment.
- **k6 binary was not executed.** It cannot be installed in this offline environment.

---

## 5. Open Items and Next Steps

1. **Configure repository secrets** `CASHFREE_PG_APP_ID`, `CASHFREE_PG_SECRET_KEY`, and `CASHFREE_PG_WEBHOOK_SECRET`. Trigger `sandbox-smoke.yml` manually. If it passes, uncomment the weekly schedule.
2. **Trigger `loadtest.yml` manually** with `include_soak=false`. If k6 installs and the fake gateway routes correctly, uncomment the nightly schedule.
3. **Close the coverage gaps** (payment –31.6%, auth –14.4%, api –11.0%, finance –7.4%). Write tests package by package, measure after each package, then enable `-strict` in CI to enforce the aspirational 85/85/80/75 targets.
4. **Provision a staging environment** to unblock Track Q.
5. **Drop `payment_collection_mode`** from the `properties` table in a future migration (after the column has been absent from application code for one full release cycle).
