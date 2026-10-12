# pg-go (div_dev) — Backend Test Sufficiency Review

Date: 2026-10-12. Input: `pg-go-div_dev__18_.zip` (643 files).
Method: static reading of code, tests, CI workflows, and `docs/wayfinder`.

## 0. Limits of this review (read first)

| Limit | Effect |
| :--- | :--- |
| No Go toolchain in the sandbox. No access to the Go module proxy. | I did **not** run `go test`, `go vet`, or `-race`. Every statement below comes from reading code. |
| Coverage percentages were **not measured**. | I report the thresholds the gate enforces. I do not report actual coverage. |
| The upload has no `.git` directory. | I could not check history, branches, or CI run results. |
| Route-to-test mapping is a text heuristic. | A route listed as "no evidence" may be tested through a helper my patterns missed. Treat the list as a review queue. |

## 1. Verdict

The test suite is **strong and mostly sound**. It is **not complete**.

- Money rail, ledger, concurrency, and role gates are tested well.
- Ten specific gaps remain. None is a confirmed exploitable defect.
- Three gaps matter most: webhook replay parity (G2), 42 routes without feature tests (G3), and coverage gates that are far below the repo's own spec (G4).

## 2. Size and shape (counted, not estimated)

- 223 source files (51,727 lines). 200 test files (50,428 lines). 68 migrations.
- CI (`ci.yml`) runs: `-race`, `REQUIRE_DB=1`, skip audit, coverage gate, 3 SQL integrity suites, 5 fuzz targets (30 s each), `go vet`, gitleaks, gosec, govulncheck.
- `test.yml` and `static-analysis.yml` are `workflow_dispatch` only.

## 3. Verified strengths

1. **Route × role matrix** (`internal/api/route_role_matrix_test.go`). It walks every registered gin route. It uses a hard-coded role table, not code under test. A route it cannot classify fails the test.
2. **Object-level isolation tests exist**: `TestFinanceCrossPropertyIDORGuards`, `TestGamificationCrossPropertyIDORGuards`, `TestSamePropertyTenantIsolation`, `TestTenantKYCReturn_ForeignTenant_Returns404`, `TestLabA_RLSIsolationFailure`, scope-claim tests.
3. **Money correctness**. `ParseRupeesToPaise` avoids floats. `coercePaise` rejects `float64` (INV-014). Property tests check double-entry conservation. An E2E journey test covers join → due → checkout → webhook → ledger → vacate.
4. **Concurrency and idempotency**. About 50 tests, including concurrent webhooks, poller/webhook race, double-tap order creation, concurrent expense payments, and OTP/token single-use races.
5. **Auth hardening tests**: refresh-token family rotation, replay detection, stale `token_version`, step-up OTP replay, purpose binding, and concurrent single-winner.
6. **Skip discipline**. `REQUIRE_DB=1` turns silent DB skips into failures. A skip audit runs after the tests.
7. **PII-bearing endpoints are correctly scoped in code.** `OwnerTenantKYCPhoto` and `TenantIDPhoto` both return 404 when `tenant.PropertyID != propertyID`. `WaiveDue` checks property scope, then step-up/dual-control, then a domain guard.

## 4. Gaps (all verified by reading code)

| ID | Severity | Gap | Evidence |
| :--- | :--- | :--- | :--- |
| G1 | Medium | No unit tests for access-token verification. | `internal/auth/` has no `jwt_test.go`. `VerifyToken` pins HS256 and reads correct, but expired, tampered, `alg=none`, HS384/HS512, and wrong-secret cases have no regression test. Similar attacks are tested only for calendar tokens. |
| G2 | Medium | Webhook replay protection differs across four handlers, and no test covers the differences. | Settlement (`handlers_settlement.go:168`) defaults tolerance to 300 when ≤ 0 (fail-closed). Payments (`handlers_pay_webhooks.go:53`) and payouts (`handlers_payouts_webhook.go:76`) skip the check when tolerance ≤ 0 (fail-open). KYC (`handlers_kyc.go:572`) verifies HMAC only and has **no timestamp check**. `config.go:120` defaults to 300 but does not reject 0 or negative. No test sets tolerance to 0 or sends a stale KYC timestamp. Idempotency may limit impact; I did not verify replay-after-state-change for KYC. |
| G3 | Medium | 42 of ~205 routes have no feature-test evidence (heuristic). | See Appendix A. They include `OwnerTenantKYCPhoto` (zero tests in repo), `DueToken` (pay-link mint, zero tests), `WaiveDue` (only a domain-rule test), `PostReimbursement` (service tested, HTTP not), notification read endpoints, `AuthRefresh` HTTP handler, and 14 owner finance/ROI reporting routes. The matrix proves their role gate only. |
| G4 | Medium | Coverage gate is far below the repo's own spec, and key packages are not tracked. | `scripts/check-coverage.go` enforces floors: finance 76, auth 64, payment **51.5**, api 62, jobs 60, gamification 35, postgres 35, notification 14, tenant 10. The spec (`TEST_READINESS_SPECIFICATION.md` Gate 02) says 85/80/85/75. Aspirational targets are report-only. Packages with **no floor**: billing, cashfree, kyc, domain, crypto, collector, csv, aadhaar, magiclink, join, search, attendance. |
| G5 | Low–Med | Public-route check in the matrix is weak. | In `TestRouteRoleAuthorizationMatrix` the `anonOK` branch fails only on **403**. A public route (for example a webhook) moved behind auth would return 401 and pass. Some public routes can legitimately return 401, so the fix must use the middleware's own error codes. |
| G6 | Low | Parse errors dropped in two webhook parsers. | `cashfree/webhook.go:169` (payment failed) and `:266` (dispute amount) use `paise, _ := ParseRupeesToPaise(...)`. A malformed amount becomes 0 instead of a dead-letter. This conflicts with the repo's fail-closed rule. |
| G7 | Medium | Three spec gates are **spec-only**. | Gate 08 (crash/fault injection), Gate 03 Schemathesis, and OWASP ZAP appear only in `docs/wayfinder/`. No code or workflow implements them. |
| G8 | Low–Med | Gate 12 job-binary tests cover one negative case. | All 13 `cmd/*` tests are the same 33-line "exit non-zero with empty environment" test. No success path, flag, or invalid-flag test exists. Job logic is covered in `internal/jobs` (37 tests), so risk is the binary wiring. |
| G9 | Low | Gate 14 PII guard covers one package. | Only `internal/kyc/privacy_pii_test.go` scans logs. The spec says all packages. |
| G10 | Low | Float in an API money field. | `handlers_owner_dashboard.go:628` returns `float64(totalPaise)/100.0` as `total_amount_rupees`. Display only. It conflicts with the "integer paise" rule. Changing it affects the frontend contract. |

Also noted (not a gap): login flows mint a 30-day `IssueToken` that the HTTP handlers discard (`_, user, err :=`). Real sessions use 15-minute access tokens plus refresh tokens. The minted token is dead work.

## 5. Spec gate status (verified against code)

| Gate | Status | Note |
| :--- | :--- | :--- |
| 01 Skip audit | Implemented | In `ci.yml`. I did not read `audit_skips.go`. |
| 02 Coverage floor | **Partial** | Floors below spec. See G4. |
| 03 API contract | Partial | `contract_test.go` and `routes_outcome_test.go` exist. Schemathesis missing. See G3, G7. |
| 04 E2E money | Implemented | `TestJourney_...`, `TestE2E...`. |
| 05 Webhook replay/order | Implemented for payments | Parity gaps in G2. |
| 06 Authz matrix | Implemented | Auth layer only. Object level covered by separate tests. See G3, G5. |
| 07 Migrations | Partial | Lock check applies to migrations ≥ 065 only. |
| 08 Crash/recovery | **Not implemented** | G7. |
| 09 Load/soak | Implemented | `loadtest.yml`, scheduled. |
| 10 Fuzz | Partial | 5 targets, 30 s on PR. Spec asks for nightly runs too. |
| 11 Sandbox smoke | Implemented | Scheduled. |
| 12 Job binaries | **Partial** | G8. |
| 13 Time/calendar | Implemented | `billing/calendar_invariants_test.go`, `prorate_test.go`. |
| 14 PII guards | Partial | G9. |

## 6. Not verified

- Actual test results, race-detector results, coverage numbers.
- Whether CI is green (spec notes ~10 failed runs earlier; `deploy.yml` unreviewed).
- Frontend (`pg-react`) tests. Not in the upload.
- Real Cashfree behavior. Only fakes and sandbox smoke are referenced.
- Replay-after-state-change safety of the KYC webhook service path.
- Content of `AUDIT_REPORT.md` and `REMEDIATION_TRACEABILITY_MATRIX.md` against current code.

## Appendix A — Routes with no feature-test evidence (heuristic)

Owner: invite, invite/rotate, join-requests/:id/reject, tenants/:id/attach-phone, tenants/:id/prorate, tenants/:id/id-photo, tenants/:id/kyc/photo, dues/:id/waive, dues/:id/pay, dues/:id/token, events, reconciliation, finance/summary, finance/ledger, finance/advances, finance/reimbursements (POST), finance/variance-bridge, finance/imports, roi, roi/recovery, roi/break-even, roi/scenario (POST), insights, coi, forecast.
Manager: inspections/items/:id/resolve, finance/today, finance/meal-prep (POST).
Tenant: inspections, inspections/items/:id/dispute, meal-rsvp (GET, POST), hazards (POST), violations, referrals (GET, POST).
Shared: PATCH notifications/:id/read, PATCH notifications/read-all.
Public/auth: GET join/invite/:code, POST auth/otp/request, POST auth/refresh, GET join/me.
