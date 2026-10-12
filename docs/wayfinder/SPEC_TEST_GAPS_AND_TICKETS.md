# Spec: Close the Backend Test Gaps (G1–G10)

Source: `PG-GO_TEST_SUFFICIENCY_REVIEW.md`. Standard: ASD-STE100, Karpathy small steps, "antislop" rules (fail closed, integer paise, minimal diffs, table-driven tests).
Place tickets in `docs/wayfinder/tickets/` using the existing `track_*` naming.

## Goal

Every gap in the review has a failing test first, then a minimal fix if code must change. CI stays green. No new float money. No behavior change without a decision below.

## Decisions needed from the owner

| ID | Question | Default if you do not answer |
| :--- | :--- | :--- |
| D1 | A stale timestamp on the KYC webhook: return 401, or return 200 and log (the KYC handler's current style)? | 200, warn log, no processing. |
| D2 | Create an OpenAPI file so Schemathesis can run (T9)? | T9 stays blocked. |
| D3 | Replace `total_amount_rupees` (float) with an integer `total_amount_paise` field? This changes the frontend contract. | T11 stays blocked. |

## Task graph

Frontier (no blockers): T0, T1, T2, T3, T4a, T4b, T4c, T4d, T5, T7, T10.
Blocked: T6 after T0. T8 after T7. T9 after D2. T11 after D3.

### T0 — Measure the real baseline
Run `go test -race -coverprofile=coverage.out ./...` with `REQUIRE_DB=1` and a Postgres 16 + pgvector service. Record per-package coverage, skip-audit result, and `go vet`. Save to `docs/wayfinder/BASELINE_COVERAGE.md`.
Done when: numbers exist for every package. Blocks T6.

### T1 — Access-token verification tests (G1)
Add `internal/auth/jwt_test.go`. Table-driven. Cases: valid; expired; wrong secret; tampered payload; tampered role claim; `alg=none`; HS384; HS512; empty string; garbage string; missing `exp`. Assert `VerifyToken` returns an error for every bad case and correct claims for the valid case.
Done when: tests pass. Mutating the HS256 check in `jwt.go` makes a test fail.

### T2 — Webhook replay parity (G2) — decision D1
Red tests first. For payments, payouts, settlement, and KYC handlers: (a) stale timestamp is refused; (b) tolerance 0 does not disable the check.
Fix: make payments and payouts default to 300 when tolerance ≤ 0 (match settlement). Reject tolerance ≤ 0 in `config.Load`. Add the timestamp check to the KYC webhook per D1.
Done when: all four handlers refuse a stale replay in tests, and `WEBHOOK_TIMESTAMP_TOLERANCE_SEC=0` fails at startup or behaves as 300.

### T3 — Matrix: public routes (G5)
In `TestRouteRoleAuthorizationMatrix`, for each public path assert the response is not one of the auth middleware's own codes (use the six codes in `TestMiddlewareGuards_AllSixCodes`). Keep a small allow-list for handler-level 401s (for example refresh, logout, calendar token).
Done when: moving `/webhooks/cashfree` behind auth makes the test fail.

### T4a — Sensitive and money-moving owner routes (G3)
Tests for: `OwnerTenantKYCPhoto`, `TenantIDPhoto`, `WaiveDue`, `PostReimbursement`, `DueToken`, `OwnerDuePay`, `TenantProrate`, `TenantAttachPhone`, `RejectJoin`, `OwnerRotateInvite`, `OwnerInvite`.
Each: happy path; foreign-property → 404; wrong role → 403. `WaiveDue` also: no step-up → refused; paid or non-waivable due → 400.

### T4b — Owner reporting routes (G3)
`FinanceSummary`, `FinanceLedger`, `ListAdvances`, `FinanceVarianceBridge`, `ListExpenseImports`, `OwnerROI*`, `InsightsSummary`, `OwnerCOI`, `OwnerForecast`, `Reconciliation`, `ListEvents`.
Each: response shape; scoped to the caller's property; summary totals equal a plain SQL `SUM` baseline on seeded data.

### T4c — Tenant and manager self-service routes (G3)
`TenantInspections`, `TenantDisputeInspectionItem`, `TenantGetMealRSVP`, `TenantSubmitMealRSVP`, `TenantReportHazard`, `TenantViolations`, `TenantReferrals`, `ManagerResolveInspectionItem`, `ManagerFinanceToday`, `PostMealPrep`. Plus `MarkNotificationRead` and `MarkAllNotificationsRead`: a user cannot mark another user's notification.

### T4d — Auth HTTP handlers (G3)
`AuthRefresh`: valid; replayed token → 401; revoked session; malformed body. `OTPRequest`: rate limit; same response for known and unknown phone. `LookupInvite`, `JoinMe`.

### T5 — Webhook parse errors fail closed (G6)
Red tests: a payment-failed payload and a dispute payload with a malformed amount. Expect a parse error and dead-letter handling, not amount 0. Fix `cashfree/webhook.go:169` and `:266`.

### T6 — Coverage gate (G4) — blocked by T0
Add floors for billing, cashfree, kyc, domain, crypto, collector, csv, aadhaar, magiclink, join, search, attendance, set to the T0 measured value minus 1 point. Raise payment's floor in steps toward 85. Move to `-strict` per package as each reaches its target.
Done when: `check-coverage.go -require-all` fails if any listed package drops.

### T7 — Job binary tests (G8)
For each `cmd/*`: `--help` exits 0; an invalid flag exits non-zero; a run against the CI database exits 0 and is safe to run twice.

### T8 — Crash and recovery (G7) — blocked by T7
Reuse the T7 subprocess harness. Kill `cmd/billing-cycle` mid-batch, rerun, and assert no duplicate dues and a balanced ledger. Stop the outbox worker mid-event and assert recovery. Add a nightly workflow.

### T9 — Contract fuzz and ZAP baseline (G7) — blocked by D2
Add OpenAPI. Add nightly Schemathesis and ZAP baseline against a sandbox server with a fake gateway.

### T10 — PII guard breadth (G9)
Extend the Gate 14 log-capture test to the API handlers that touch phone and Aadhaar data.

### T11 — Integer paise on dashboard (G10) — blocked by D3
Add `total_amount_paise`. Keep the old field for one release. Update `CONTRACT.md` and the contract test.

## Acceptance for the whole spec

- T1–T5, T7, T10 merged with tests that fail before the fix.
- CI is green with `-race` and `REQUIRE_DB=1`.
- Skip audit reports zero DB skips.
- `check-coverage.go -require-all` passes with the T6 floors.
