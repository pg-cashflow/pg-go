# PG Cashflow — Test Readiness Specification & Verification Gates

> **Authoritative Specification**: Quality Engineering & Testing Framework  
> **Methodology**: ASD-STE100 (Simplified Technical English) & Karpathy Test Discipline  
> **Repository Target**: `github.com/pg-cashflow/pg-go`  
> **Scale**: 1–10 Properties · 50–500 Tenants · 5–20 Staff  
> **Primary Rule**: Financial correctness and zero ledger drift outrank raw speed.

---

## 1. Quality Objectives and Operational Targets

We set strict targets for all test gates.
We enforce zero integer-paise drift across all accounts.
Every financial balance must balance exactly.

| Dimension | Operational Target | Enforcement Mechanism |
| :--- | :--- | :--- |
| **Scale Envelope** | 50–500 tenants, 1–10 properties, 5–20 staff | Configurable seed generators |
| **Real Peak Rate** | 1–5 requests per second during due dates | Baseline telemetry |
| **Load Test Target** | 50–100 requests per second (10–20× peak) | k6 open-model arrival rate |
| **API Latency** | p95 < 500 ms; Checkout intent p95 < 800 ms | k6 thresholds + CI failure gate |
| **Webhook Processing** | Acknowledge in < 2 s (p99); zero duplicate effects | Asynchronous deduplication |
| **HTTP Error Rate** | < 1.0% failure rate under peak load | CI gate threshold |
| **Ledger Inbalance** | Exactly 0 paise drift ($\sum\text{Debits} == \sum\text{Credits}$) | SQL invariant assertions |
| **Disaster Recovery** | RPO $\le$ 5 minutes; RTO $\le$ 1 hour | Automated backup restore drill |

---

## 2. Test Execution Architecture

We organize tests into four distinct operational tiers.
Fast tests run on every pull request.
Slow and non-deterministic tests run on schedules.

```
 PR Gate (< 10 min)        Nightly Schedule           Weekly / Pre-Release       Go-Live Gate
 -------------------       --------------------       --------------------       -------------------
 Gate 01: Skip Audit       Gate 03: Schemathesis      Gate 08: DR Restore Drill  External Pentest
 Gate 02: Coverage Floor   Gate 08: Fault Injection   Gate 09: 1h Soak Run       Live Reconcile Drill
 Gate 03: API Contract     Gate 09: k6 Load Test      Gate 11: Gateway Smoke     Staging Parity Signoff
 Gate 04: E2E Money Flow   Gate 10: Native Fuzzing    Gate 14: Privacy Audit     Rollback Drill
 Gate 05: Webhook Replay   Gate 12: Full Job Suite    DB Query Plan Review       Operator Alert Check
 Gate 06: Authz Matrix     Gate 13: Leap Year Suite   OWASP ZAP Active Scan      0-Paise Balance Audit
 Gate 07: Migration Lint   OWASP ZAP Baseline Scan    
 Gate 10: Short Fuzz (30s) Continuous Outbox Worker   
```

---

## 3. ASD-STE100 Rules for All Tests

All test specifications and test code must follow these rules:

1. Write short and direct sentences.
2. Limit each sentence to fewer than 25 words.
3. Use active voice only. Do not use passive voice.
4. Use one clear command per sentence.
5. Use standard action verbs: `Run`, `Inspect`, `Compare`, `Assert`, `Verify`, `Send`, `Reject`, `Stop`.
6. State preconditions clearly before every action.
7. State expected outcomes explicitly after every action.

---

## 4. Karpathy Test Recipe

Apply this six-step recipe to each verification gate:

1. **Look at the data first:**
   - Inspect raw database rows with direct SQL queries.
   - Inspect raw JSON payloads from HTTP requests.
   - Verify ledger entries by hand before writing automated tests.

2. **Check state at initialization:**
   - Migrate a fresh database.
   - Check that all ledger journal sums equal zero.
   - Assert that no orphan records exist at startup.

3. **Overfit one example:**
   - Create one property, one tenant, and one rent due.
   - Execute one payment flow end to end.
   - Make this single flow 100% green before testing variations.

4. **Compare with a dumb baseline:**
   - Compare complex engine calculations with a naive SQL `SUM()`.
   - Compare fee allocations with a simple reference calculation.
   - Fail the test if the engine deviates from the simple baseline.

5. **Fix seeds:**
   - Use fixed pseudo-random seeds for randomized tests.
   - Ensure every test run is deterministic and reproducible.
   - Record seed values on test failure.

6. **Change one thing at a time:**
   - Isolate each test parameter.
   - Modify only one variable per test case.
   - Isolate failures quickly to single code units.

---

## 5. The 14 Verification Gates Summary

| Gate # | Name | Tool / Framework | Target Package | Tier |
| :--- | :--- | :--- | :--- | :--- |
| **Gate 01** | Test Skip Audit | Go Test Parser + `REQUIRE_DB=1` | All 31 packages | PR |
| **Gate 02** | Coverage Floor | `go test -coverprofile` + Gating script | `finance`, `payment`, `api`, `auth` | PR |
| **Gate 03** | API Contract Test | Go `httptest` + OpenAPI / `CONTRACT.md` | `internal/api` | PR + Nightly |
| **Gate 04** | E2E Money Lifecycle | Real HTTP + Postgres | `internal/api`, `internal/finance` | PR |
| **Gate 05** | Webhook Ordering | Fake Cashfree Server + Race Detector | `internal/api`, `internal/cashfree` | PR |
| **Gate 06** | Authz & IDOR Matrix | Table-Driven Test Generator | All route handlers in `router.go` | PR |
| **Gate 07** | Migration Invariants | Migration Runner + Lock Linter | `migrations/*.sql`, `cmd/migrate` | PR |
| **Gate 08** | Crash & Recovery | Process Killing + Toxiproxy Faults | `cmd/*`, `internal/finance` | Nightly |
| **Gate 09** | Load & Soak Testing | k6 Open-Model Arrival + Post SQL Check | `/api/auth`, `/api/tenant`, `/api/pay` | Nightly + Weekly |
| **Gate 10** | Native Go Fuzzing | Go native `testing.F` | Webhook, CSV, Aadhaar, Phone | PR + Nightly |
| **Gate 11** | Gateway Sandbox Smoke | Scheduled `cmd/sandbox-smoke` | Cashfree Sandbox PG | Weekly |
| **Gate 12** | Background Job Tests | Subprocess Exit Code & Flag Harness | `cmd/*`, `internal/jobs` | PR |
| **Gate 13** | Time & Calendar Tests | Mock Clock (`timeutil.Clock`) | `internal/billing`, `internal/finance`| PR |
| **Gate 14** | Privacy & PII Leak Guards | Memory & Log Pattern Scanner | All packages + log sinks | PR |

---

## 6. Detailed Specifications for Each Gate

### Gate 01: Test Skip Audit
- **Goal**: Prevent silent test skips from hiding regressions.
- **Problem**: 127 calls to `t.Skip` exist across tests when `DATABASE_URL` is unset or `-short` is active.
- **Requirement**: When `REQUIRE_DB=1` is set in CI, any call to `t.Skip` due to database absence must fail the test suite.
- **Verification**: Run `go test -v ./...` in CI. Parse output. Fail immediately if any test outputs `--- SKIP`.

### Gate 02: Coverage Floor
- **Goal**: Ensure core financial and security paths maintain high test coverage.
- **Floor Thresholds**:
  - `internal/finance`: Minimum 85% statement coverage.
  - `internal/payment`: Minimum 85% statement coverage.
  - `internal/auth`: Minimum 80% statement coverage.
  - `internal/api`: Minimum 75% statement coverage.
- **Verification**: Generate `coverage.out` via `go test -coverprofile=coverage.out ./...`. Enforce thresholds in CI.

### Gate 03: API Contract Tests
- **Goal**: Protect frontend integration from breaking changes in HTTP routes, shapes, and error codes.
- **Specification Source**: `CONTRACT.md` Rev 13 and `error-codes.json`.
- **Requirement**: Every registered route in `internal/api/router.go` must have a contract test.
- **Verification**: Verify response JSON structure. Verify error code format (`{ "error": "...", "code": "domain.reason" }`).

### Gate 04: End-to-End Money Lifecycle
- **Goal**: Verify complete financial transactions across HTTP API and real PostgreSQL.
- **Lifecycle Sequence**:
  1. Join request and approval.
  2. Rent due generation.
  3. Payment intent creation.
  4. Webhook settlement.
  5. Ledger posting.
  6. Partial refund allocation.
- **Invariant**: Assert $\sum\text{Debits} == \sum\text{Credits}$ at every step. Assert zero ledger drift.

### Gate 05: Webhook Replay and Ordering
- **Goal**: Prevent duplicate credits or race conditions during gateway webhook retries.
- **Risk**: Out-of-order delivery, duplicate delivery, and network delay.
- **Requirement**: Use a local fake Cashfree gateway server. Deliver duplicate webhooks concurrently.
- **Verification**: Assert idempotent HTTP 200 responses. Assert payments and ledger entries post exactly once.

### Gate 06: Authorization Matrix
- **Goal**: Prevent broken access controls and IDOR vulnerabilities.
- **Matrix Dimensions**: Every Route $\times$ Every Role (`anon`, `tenant`, `manager`, `owner`) $\times$ Property Ownership (`own`, `other`).
- **Assertion**:
  - Denied anonymous requests return 401 Unauthorized.
  - Denied cross-role requests return 403 Forbidden.
  - Cross-property tenant/manager access returns 404 Not Found (or 403 Forbidden).

### Gate 07: Migration Tests
- **Goal**: Guarantee zero deployment downtime and safe schema evolutions.
- **Requirements**:
  1. Test fresh migration on empty database.
  2. Test sequential upgrade from migration 001 to 044.
  3. Test migration re-run idempotency.
  4. Check migration SQL for dangerous exclusive locks (`ALTER TABLE` without lock timeouts).

### Gate 08: Crash and Recovery
- **Goal**: Guarantee ledger integrity when processes or connections crash.
- **Scenarios**:
  1. Kill `server` process during active payment transaction.
  2. Kill `cmd/billing-cycle` worker mid-batch.
  3. Inject network disconnections using Toxiproxy.
- **Verification**: Confirm transaction rollback in PostgreSQL. Confirm outbox worker recovers unmirrored events.

### Gate 09: k6 Load and Soak Testing
- **Goal**: Validate throughput, latency, and correctness under peak concurrency.
- **Framework**: k6 using open-model arrival rates (`ramping-arrival-rate`).
- **Scenarios**:
  - Scenario 1: Tenant login and dashboard read mix (40 RPS).
  - Scenario 2: Checkout intent creation and signed webhook processing (20 RPS).
  - Scenario 3: 1-hour soak test at moderate load (15 RPS).
- **Critical Requirement**: Never call live Cashfree in load tests. Use a local fake server.
- **Post-Run Invariant Check**: Run SQL audit after test completion:
  $$\sum\text{Debits} == \sum\text{Credits}$$
  Assert zero duplicate payments. Assert zero over-applied dues.

### Gate 10: Native Go Fuzzing
- **Goal**: Uncover panics and parsing bugs using Go native fuzzing (`testing.F`).
- **Targets**:
  1. Cashfree webhook JSON parser (`internal/cashfree`).
  2. Bank statement CSV parsers (`internal/finance` SBI/HDFC).
  3. UIDAI Aadhaar secure QR parser (`internal/aadhaar`).
  4. Phone number validator (`internal/domain`).

### Gate 11: Sandbox Gateway Smoke Test
- **Goal**: Verify compatibility with real Cashfree sandbox APIs without slowing down PRs.
- **Strategy**: Run `cmd/sandbox-smoke` on a scheduled weekly CI job.
- **Isolation**: Real external sandbox calls are banned from standard PR gates.

### Gate 12: Background Job Binaries Tests
- **Goal**: Validate flag parsing, database connections, and exit codes for all 10+ cron binaries.
- **Target Binaries**:
  `cmd/billing-cycle`, `cmd/reminder`, `cmd/cashfree-poll`, `cmd/digilocker-reconcile`,
  `cmd/kyc-expiry`, `cmd/financial-summary`, `cmd/kpi-snapshot`, `cmd/gamification-cycle`,
  `cmd/search-reindex`, `cmd/migrate`.
- **Verification**: Run each binary with `--help`, invalid flags, and mock environments. Assert exit code 0 on success and non-zero on failure.

### Gate 13: Time and Calendar Tests
- **Goal**: Prevent edge-case billing errors caused by calendar variations.
- **Clock Abstraction**: Inject mock clocks into billing and finance services.
- **Test Cases**:
  - Month-end day clamping (days 29, 30, and 31 clamped to 28 for February).
  - Leap year transition (February 29 in leap years).
  - Indian Standard Time (IST) midnight date boundaries (UTC offset +05:30).

### Gate 14: Privacy and PII Leak Guards
- **Goal**: Prevent storage or logging of sensitive Aadhaar numbers and raw biometric data.
- **Compliance Target**: Digital Personal Data Protection (DPDP) Act and UIDAI regulations.
- **Verification**:
  - Inspect application logs, error strings, and JSON responses for regex patterns:
    - 12-digit Aadhaar numbers (`\b[2-9]{1}[0-9]{3}[0-9]{4}[0-9]{4}\b`).
    - Raw 10-digit mobile numbers in unmasked contexts.
  - Assert that all database records store masked references or salted hashes.

---

## 7. Open Technical Issues Resolution

1. **Go Version Alignment**:
   - `go.mod` specifies `go 1.26.0`.
   - `.github/workflows/ci.yml` specifies `go-version: '1.23.2'`.
   - **Resolution**: Align CI workflow to build with the same major Go toolchain version specified in `go.mod` or pin `go.mod` to the active LTS toolchain (e.g., Go 1.23.x / 1.24.x).

2. **Package Test Coverage Gaps**:
   - `internal/domain`, `internal/events`, and `internal/gamification` have fewer tests than core finance packages.
   - **Resolution**: Core business logic packages must meet Gate 02 coverage floors. Pure DTO and event definition packages require schema validation tests.
