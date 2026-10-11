# ADR-028: Security Hardening, Invariant Remediation, and Documentation Reconciliation

**Status:** Accepted  
**Date:** 2026-10-11  
**Basis:** Security Audit, Vulnerability Remediation, and Invariant Verification  
**Deciders:** Architecture / Core Engineering  
**Standard:** ASD-STE100 (Simplified Technical English)  
**Method:** Empirical First-Principles Verification (Karpathy Method)  

---

## 1. Context

Codebase auditing and static analysis identified several security vulnerabilities, financial invariant violations, and documentation drifts:

1. **KYC Webhook Forgery Vulnerability (High):**
   - In `internal/cashfree/client.go`, `VerifyWebhookHMAC` calculated HMAC without checking if `secret` or `signature` was empty. An empty secret and empty signature resulted in a matching HMAC evaluation.
   - In `internal/api/handlers_kyc.go`, `CashfreeKYCWebhook` invoked `VerifyWebhookHMAC(h.CashfreeSecret, ...)` without verifying whether `h.CashfreeSecret` was configured. In QR-only mode or when webhook secrets were omitted, unauthenticated callers could forge KYC events.

2. **Blind SSRF via Web Push Subscriptions (High):**
   - In `internal/push/service.go`, `Subscribe` accepted arbitrary `endpoint` URLs without protocol or hostname validation.
   - The notification dispatcher (`Send`) issued outbound HTTP POST requests to these arbitrary URLs via `webpush.SendNotificationWithContext`. An attacker holding a valid magic-link payment token could target loopback interfaces, cloud metadata endpoints (`169.254.169.254`), or intranet services.

3. **Cross-Tenant Push Subscription Rebinding (Medium):**
   - In `internal/postgres/otp_repo.go`, `PushRepo.Upsert` executed:
     `ON CONFLICT (endpoint) DO UPDATE SET tenant_id=EXCLUDED.tenant_id, p256dh=EXCLUDED.p256dh, auth=EXCLUDED.auth RETURNING id`.
   - This allowed an existing subscription endpoint to be reassigned to a different tenant, replacing encryption keys.

4. **Floating-Point Gateway Amount Coercion (INV-014 Violation) (Medium):**
   - In `internal/cashfree/settlement.go`, `json.Unmarshal` decoded numbers into IEEE-754 `float64` values.
   - In `coercePaise`, string parsing fell back to `strconv.ParseFloat(s, 64)`. Amounts with more than two decimal places (such as `"5500.505"`) or scientific notation (`"1e3"`) were silently rounded instead of rejected, violating INV-014.

5. **Silent Zero Occupancy Metrics (INV-015 Violation) (Medium):**
   - In `internal/api/handlers_owner_dashboard.go`, `computeOccupancyMetrics` ignored errors from `ListFloors`, `ListRooms`, and `ListByProperty` (`floors, _ = ...`). Database failures resulted in zero-occupancy numbers returned with HTTP 200 OK.
   - Furthermore, `OwnerDashboardSummary` concatenated internal error details (`err.Error()`) into error responses, disclosing internal error messages.

6. **Metrics Authorization Token Leak in Query String (Low-Med):**
   - In `internal/api/handlers_metrics.go`, `Metrics` accepted authentication tokens via `?token=` query parameters. Query parameters risk exposure in web server logs, proxy access logs, and browser history.

7. **Invite Lookup Rate Limit Absence (Low-Med):**
   - In `internal/api/router.go`, `/api/join/invite/:code` lacked per-IP rate limiting, allowing enumeration attempts against invite codes.

8. **INV-002 Documentation Drift (Low-Med):**
   - Documentation in `FINANCIAL_INVARIANTS.md`, `EVIDENCE_VERIFICATION_REPORT.md`, and `ENGINEERING_FLOW.md` referenced a non-existent database constraint `uq_payments_normalized_utr` on a `normalized_utr` column in `migrations/046`.
   - The actual database schema enforces uniqueness via `payments.upi_txn_id UNIQUE` in `migrations/001_initial.sql`.

9. **Stray Artifact Cleanup (Trivial):**
   - A stray `cover_auth` coverage file existed at the repository root.

---

## 2. Decisions

### D1. Fail-Closed HMAC Verification and KYC Webhook Configuration Guard
- Update `VerifyWebhookHMAC` in `internal/cashfree/client.go` to return `false` immediately if `secret == ""` or `signature == ""`.
- Update `CashfreeKYCWebhook` in `internal/api/handlers_kyc.go` to return HTTP 503 Service Unavailable (`"kyc webhook secret not configured"`) when `h.CashfreeSecret == ""`.

### D2. Push Endpoint Host Allowlisting (Anti-SSRF)
- Implement `validatePushEndpoint` in `internal/push/service.go`.
- Restrict push endpoint URLs strictly to `https` scheme, default port 443, no user credentials, and an allowlist of trusted browser push vendors:
  - `fcm.googleapis.com` (Google)
  - `push.apple.com` and `*.push.apple.com` (Apple)
  - `notify.windows.com` and `*.notify.windows.com` (Microsoft WNS)
  - `push.services.mozilla.com` and `*.push.services.mozilla.com` (Mozilla)
- Define sentinel error `ErrInvalidPushEndpoint` in `internal/push`.
- Register error code `CodePushEndpointInvalid` ("push.endpointInvalid") in `internal/apierr/codes.go`.
- Map `push.ErrInvalidPushEndpoint` to HTTP 400 Bad Request in `PaymentPushSubscribe` (`internal/api/handlers_public.go`).

### D3. Prevent Cross-Tenant Push Subscription Rebinding
- Update `PushRepo.Upsert` in `internal/postgres/otp_repo.go` to add `WHERE push_subscriptions.tenant_id = EXCLUDED.tenant_id` to the conflict update clause.
- Return sentinel error `ErrPushEndpointOwned` if an existing endpoint belongs to another tenant.
- Register error code `CodePushEndpointConflict` ("push.endpointConflict") in `internal/apierr/codes.go`.
- Map `postgres.ErrPushEndpointOwned` to HTTP 409 Conflict in `PaymentPushSubscribe`.

### D4. Strict Exact-Decimal Gateway Parsing (INV-014)
- Introduce `unmarshalExactNumbers` helper using `json.NewDecoder(...).UseNumber()` and disallowing trailing bytes.
- Parse all settlement payloads (`parseSingleOrderSettlement`, `ParseSettlementWebhook`) using `unmarshalExactNumbers`.
- In `coercePaise` in `internal/cashfree/settlement.go`:
  - In `case string`: parse strictly using `ParseRupeesToPaise`. Remove `strconv.ParseFloat` fallback completely.
  - In `case json.Number`: parse strictly via `ParseRupeesToPaise`.
  - In `case float64, float32`: reject with explicit error `"floating-point money not allowed per INV-014"`.

### D5. Transparent Non-Zero Failure Propagation (INV-015)
- Update `computeOccupancyMetrics` in `internal/api/handlers_owner_dashboard.go` to return `(OccupancyMetrics, error)`.
- Propagate database errors from `ListFloors`, `ListRooms`, and `ListByProperty`.
- In `OwnerOccupancy` and `OwnerDashboardSummary`, return HTTP 500 Internal Server Error when occupancy computation fails.
- Remove raw `err.Error()` string interpolation from `OwnerDashboardSummary` error responses.

### D6. Header-Only Metrics Authentication
- Remove `c.Query("token")` query parameter fallback in `Metrics` (`internal/api/handlers_metrics.go`).
- Require `Authorization: Bearer <token>` header when `METRICS_TOKEN` is configured.

### D7. Rate Limit Invite Code Lookups
- Apply `ipRateLimit(10.0/60, 20)` middleware to `/api/join/invite/:code` in `internal/api/router.go`.

### D8. Reconcile INV-002 Documentation
- Update `FINANCIAL_INVARIANTS.md`, `EVIDENCE_VERIFICATION_REPORT.md`, and `ENGINEERING_FLOW.md` to document the actual constraint: `payments.upi_txn_id UNIQUE` (`migrations/001_initial.sql`).

### D9. Remove Stray Artifacts
- Delete `cover_auth` from repository root.

---

## 3. Verification Matrix

| Area | Check | Tool / Test | Result |
| :--- | :--- | :--- | :--- |
| KYC HMAC Empty Secret | Empty secret fails closed | `TestVerifyWebhookHMAC` (`internal/cashfree`) | Pass |
| KYC Webhook Missing Secret | Missing secret returns 503 | `TestCashfreeKYCWebhook_UnconfiguredSecret_Returns503` (`internal/api`) | Pass |
| Push SSRF Validation | Allowlisted vs invalid hosts | `TestPushService_SubscribeValidation` (`internal/push`) | Pass |
| Push Rebinding Protection | Sentinel error on conflict | `TestPaymentPushSubscribe_Errors` (`internal/api`) | Pass |
| INV-014 Float Rejection | Float inputs rejected | `TestCoercePaise_FailClosed` (`internal/cashfree`) | Pass |
| INV-014 Trailing JSON | Trailing JSON rejected | `TestUnmarshalExactNumbers_TrailingData` (`internal/cashfree`) | Pass |
| INV-015 Failure Transparency | Store failure returns 500 | `TestOwnerOccupancy_FailsClosedOnStoreError` (`internal/api`) | Pass |
| Metrics Auth | Query string token rejected | `TestMetricsEndpoint_AuthClosedByDefault` (`internal/api`) | Pass |
| Invite Rate Limit | Route matrix integrity | `TestRoute*` (`internal/api`) | Pass |
| Dashboard Error Sanitization | Internal error details omitted | `TestOwnerDashboard_InternalErrorsAreNotEchoed` (`internal/api`) | Pass |
| Package Compiles | Full internal packages | `go test ./...` | Pass |
| Concurrency Race Detection | Zero data races | `go test -count=1 -race ./...` (MinGW-w64 GCC 16.1.0) | Pass |
| Test Skip Audit | Zero tests skipped | `go test -count=1 -v ./...` | Pass (0 skips) |

---

## 4. Addendum: Error Sanitization, Race Detection, and Audit Verification

1. **Dashboard Internal Error Sanitization:**
   - In `internal/api/handlers_owner_dashboard.go`, `OwnerDashboardSummary` and `OwnerMonthlyCashFlow` previously concatenated raw `err.Error()` in responses when `Finance.OperatingSummary` or `computeMonthlyCashFlow` failed.
   - All error responses in `OwnerDashboardSummary` and `OwnerMonthlyCashFlow` now use standardized `apierr.ErrorEnvelope` with generic messages (`"failed to build operating summary"`, `"failed to compute monthly cash flow"`).
   - Verified via `TestOwnerDashboard_InternalErrorsAreNotEchoed`: sensitive error strings are not returned to the client.

2. **Calendar Token Audit Logging:**
   - In `internal/api/handlers_owner_dashboard.go`, `OwnerGetCalendarToken` previously discarded the database execution error (`_, _ = h.Pool.Exec(...)`) when inserting into `property_calendar_tokens`.
   - The failure is now logged via `slog.WarnContext(c.Request.Context(), "failed to audit calendar token insert", ...)` without blocking token issuance.

3. **Concurrency Race Detector Verification:**
   - Executed `go test -count=1 -race ./...` across all 48 test packages with `REQUIRE_DB=1` connected to the live PostgreSQL instance on `localhost:5432` (`postgres://postgres:port%401@localhost:5432/pg_test?sslmode=disable`).
   - Built with MSYS2 MinGW-w64 GCC 16.1.0 (`C:\msys64\mingw64\bin\gcc.exe`).
   - Result: All 48 packages returned `ok`. Zero data races and zero build failures detected.

4. **Test Skip Audit:**
   - Executed `go test -count=1 -v ./...` under `REQUIRE_DB=1` to count all skipped tests.
   - Result: `TOTAL_SKIPS: 0`. Exactly zero tests were skipped.

5. **Completed Error Isolation and Logging Hardening:**
   - `handlers_gamification.go:1038`: Implemented sentinel classification in `internal/gamification/redemptions.go` (`ErrRedemptionNotFound`, `ErrRedemptionNotPending`). Validation and not-found states return generic HTTP 400. Unexpected and repository errors route through `respondErr(c, err)` returning HTTP 500 without leaking database details. Verified with `TestFulfilRedemption_LookupFailureIsInternal`.
   - `handlers_attendance.go:275`: Implemented `*attendance.InputError` for request validation failures in `internal/attendance/service.go`. Validation errors return HTTP 400 with the input error message. Repository database errors route through `respondErr(c, err)` returning HTTP 500.
   - `handlers_owner_dashboard.go`: Restored `slog.ErrorContext` logging across dashboard error branches (`OwnerOccupancy`, `OwnerDashboardSummary`, `OwnerMonthlyCashFlow`) to preserve operator observability while maintaining sanitized `apierr.ErrorEnvelope` client responses.
   - `handlers_owner.go:1675`, `handlers_public.go:307`, `handlers_search.go:73`: Verified safe. They strictly wrap domain validation or auth sentinels, with unexpected errors routed to `respondErr`.

---

## 5. Consequences

- Webhook verification cannot be bypassed with unconfigured or empty keys.
- SSRF vectors through web push subscriptions are blocked.
- Gateway financial values are parsed as exact decimals without floating-point precision loss.
- Occupancy metrics fail transparently with HTTP 500 rather than reporting false zero occupancy.
- Metrics credentials cannot leak into URL access logs.
- Internal database details and runtime errors cannot leak into client HTTP responses in owner dashboard endpoints.
- Concurrency invariants (INV-004, INV-005, INV-012) are empirically verified free of data races under Go's race detector.

---

## 6. Round 12 Remediation: Classifier and Test Sentinel Corrections

### 6.6 Gamification Classifier and Attendance Sanitization Corrections

- **Classifier Error Text Matching Elimination:** Removed message string matching (`strings.Contains(..., "not found")` and `"no rows"`) from `FulfilRedemption` in `internal/gamification/redemptions.go`. The service strictly classifies errors against typed sentinels `domain.ErrNotFound` and `pgx.ErrNoRows`. Database errors containing those substrings are preserved and routed as internal server errors rather than misclassified as HTTP 400 client errors.
- **Test Double Sentinel Alignment:** Corrected `memStore.GetRedemptionByID` in `internal/gamification/service_test.go` to return `domain.ErrNotFound` instead of raw string error `errors.New("redemption not found")`.
- **Database Error Text Regression Test:** Added `TestFulfilRedemption_NotFoundTextInDBErrorIsInternal` verifying that database errors containing "not found" substrings are returned as internal errors rather than misclassified as `ErrRedemptionNotFound`.
- **Attendance Work Date Layout Leak Prevention:** Reverted `work_date` parse failure message in `internal/attendance/service.go` to fixed string `"invalid work_date (expected YYYY-MM-DD)"`, preventing exposure of Go time parser internal layout strings.

### 6.7 Consumer SQLSTATE Duplicate Award Detection (Round 13)

- **Consumer Duplicate Award Error Detection:** Replaced error message string matching (`strings.Contains(..., "duplicate key")` and `"unique constraint"`) in `internal/gamification/consumer.go` (`errorsIsDuplicate`) with a typed SQLSTATE check (`errors.As(err, &pgErr) && pgErr.Code == "23505"` using `github.com/jackc/pgx/v5/pgconn`), matching the pattern used in `internal/postgres/finance_repo.go`.
- **Eliminated Unused Imports:** Removed unused `strings` package dependency from `internal/gamification/consumer.go`.
- **Unit Test Coverage:** Added `internal/gamification/consumer_duplicate_test.go` (`TestErrorsIsDuplicate_UsesSQLState`) testing real SQLSTATE 23505, 23503, error strings containing "duplicate key" without SQLSTATE, and nil error inputs.

### 6.8 Payouts Sentinel Error Classification (Round 14)

- **Exported Payout Sentinel Error:** Exported `postgres.ErrNoUnbatchedPayoutItems = errors.New("no unbatched pending payout items found")` in `internal/postgres/payout_repo.go` returned when no unbatched items exist.
- **Handler Error Classification:** Updated `CreateBatchFromUnbatched` in `internal/api/handlers_payouts.go` to match against `postgres.ErrNoUnbatchedPayoutItems` using `errors.Is(err, postgres.ErrNoUnbatchedPayoutItems)` instead of `strings.Contains(err.Error(), "no unbatched")`. Other database errors cleanly fall through to the generic 500 response.
- **Repository-Wide Message Matching Audit:** Confirmed complete elimination of `strings.Contains(err...)` and `err.Error()` message matching across all non-test packages in `internal/` and `cmd/`.
