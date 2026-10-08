# API Contract (pg-go)

**Rev 13.** HTTP source of truth for [pg-react](https://github.com/pg-cashflow/pg-react). Route list matches `NewRouter` in `internal/api/router.go`. TypeScript field shapes live in pg-react `packages/types/index.ts` (mirror of Go domain types; if they diverge, fix types to match this API).

**Out of scope:** scheduled CLIs (`billing-cycle`, `reminder`, `cashfree-poll`, `digilocker-reconcile`, `financial-summary`, `gamification-cycle`, `search-reindex`, `kyc-expiry`, `kpi-snapshot`) and env/runbooks — see [README.md](README.md).

## Base URL

- Local development: `http://localhost:8080/api` (`VITE_API_BASE_URL=http://localhost:8080/api` in pg-react `.env.local`)
- Production embedded PWA: `/api` (same origin; `VITE_API_BASE_URL=/api`)

## Conventions

- **Money:** all amounts in **paise** (integer). ₹15,000 = `1500000`.
- **Errors:** `{ "error": "message", "code": "domain.reason"? }`. The `error` string is the human-readable English message (preserving backward compatibility). The additive `code` field is an optional machine-readable dot-delimited identifier (e.g. `"preferences.invalidLocale"`, `"auth.invalidOtp"`) intended for client-side localization.
- **Locale Resolution & Accept-Language:** Every request is processed by `localization.Middleware()`. The active request locale is resolved deterministically and header-driven in-memory:
  1. `Accept-Language` header matched against supported BCP-47 tags via `golang.org/x/text/language` matcher (supporting dialect fallbacks such as `te` → `te-IN`, `kn` → `kn-IN`). If missing, malformed, or exceeding 256 bytes, falls back to default.
  2. Default platform fallback: `en-IN`.
  Authenticated user preferences from `user_preferences` are evaluated at session exchange (`POST /api/auth/firebase`, `POST /api/auth/otp/verify`) and `/api/me/preferences` to drive client sync and server-initiated dispatch, adding zero database round-trips to regular API requests.
  Supported locales in Phase 1: `en-IN`, `te-IN`, `ta-IN`, `kn-IN`.
- **Auth:** Firebase ID token exchanged at `POST /auth/firebase` for a 30-day app JWT. Subsequent calls use `Authorization: Bearer <jwt>`.
- **List envelopes:** responses wrap arrays — `{ properties }`, `{ tenants }`, `{ dues }`, `{ payments }`, `{ events }`, `{ inspections }`, `{ hazards }`, `{ rewards }`, `{ notifications }`, `{ join_requests }`, `{ payment_reports }`, `{ expenses }`, `{ capital }`, `{ advances }`, `{ budgets }`, `{ approvals }`, `{ suggestions }`, `{ referrals }`, `{ violations }`, `{ floors }`, `{ rooms }`, `{ departures }`, `{ deductions }`, `{ payees }`, `{ items }`, `{ batches }`.

## Error Code Registry

The backend emits additive machine-readable error codes across authenticated and public endpoints.
The full canonical registry of domain-qualified error codes is documented below:

<!-- ERROR_CODES_START -->
| Error Code | HTTP Status | Domain | Description |
| :--- | :--- | :--- | :--- |
| `auth.missingToken` | 401 | Auth | Missing Authorization Bearer header |
| `auth.invalidToken` | 401 | Auth | Invalid or expired Bearer JWT token |
| `auth.accessRevoked` | 403 | Auth | Tenant vacated or session revoked via token version bump |
| `auth.profileIncomplete` | 403 | Auth | Tenant profile not yet completed |
| `auth.forbidden` | 403 | Auth | Caller lacks required role or scope |
| `auth.alreadyActivated` | 403 | Auth | Tenant already activated |
| `auth.invalidOtp` | 401 | Auth | Invalid OTP code submitted |
| `auth.otpExpired` | 401 | Auth | OTP code has expired |
| `auth.otpLocked` | 401 | Auth | Too many failed attempts, OTP locked |
| `auth.rateLimited` | 429 | Auth | Too many requests, rate limited |
| `auth.noAccount` | 404 | Auth | Account not found for phone/email |
| `auth.invalidInvite` | 404 | Auth | Invalid invite code |
| `auth.firebaseNotConfigured` | 503 | Auth | Firebase Admin SDK not configured |
| `auth.emailNotVerified` | 403 | Auth | Email is not verified with Google |
| `auth.invalidFirebaseToken` | 401 | Auth | Invalid Firebase ID token or verification failed |
| `auth.unauthorized` | 401 | Auth | Unauthorized request (missing claims/user) |
| `auth.noPropertyScope` | 403 | Auth | Missing required property scope |
| `join.invalidInvite` | 404 | Join | Invalid invite code |
| `join.noPendingRequest` | 404 | Join | No pending join request found |
| `join.notFound` | 404 | Join | Join request not found |
| `join.alreadyOnboarded` | 409 | Join | Tenant already onboarded |
| `join.alreadyActive` | 409 | Join | Tenant already active |
| `join.nameRequired` | 400 | Join | Name is required |
| `join.consentRequired` | 400 | Join | Consent is required |
| `join.photoRequired` | 400 | Join | ID photo is required |
| `join.profileIncomplete` | 400 | Join | Complete profile before activation |
| `join.requestNotPending` | 400 | Join | Join request is not pending |
| `join.notAwaitingAssignment` | 400 | Join | Join request is not awaiting assignment |
| `payment.duplicateTxn` | 409 | Payment | Duplicate transaction ID |
| `payment.cashPartialNotAllowed` | 400 | Payment | Cash payment must match due amount exactly |
| `payment.dueNotOpen` | 400 | Payment | Due is not open for payment |
| `payment.noDepositDue` | 400 | Payment | No pending deposit due for this tenant |
| `payment.emptyTxnId` | 400 | Payment | Transaction ID cannot be empty |
| `payment.ambiguousMatch` | 400 | Payment | Multiple dues match amount within date window |
| `payment.noMatch` | 400 | Payment | No pending due found matching amount within date window |
| `payment.invalidAmount` | 400 | Payment | Payment amount must be a positive integer |
| `finance.duplicateRequest` | 409 | Finance | Duplicate idempotency key |
| `finance.idempotencyRequired` | 400 | Finance | Idempotency-Key header is required |
| `finance.invalidAmount` | 400 | Finance | Invalid expense or payment amount |
| `finance.invalidKind` | 400 | Finance | Invalid transaction kind |
| `finance.overpay` | 400 | Finance | Payment exceeds remaining balance |
| `finance.expenseNotPayable` | 400 | Finance | Expense cannot accept payment |
| `finance.policyExceeded` | 400 | Finance | Spend policy exceeded |
| `finance.approvalRequired` | 400 | Finance | Owner approval required before payment |
| `finance.periodNotCloseable` | 400 | Finance | Unexplained difference blocks period close |
| `finance.periodClosed` | 409 | Finance | Write targets a closed accounting period; reopening is an audited privileged act |
| `finance.periodNotReopenable` | 400 | Finance | Period is not closed or cannot be reopened |
| `finance.notFound` | 404 | Finance | Financial entity not found |
| `finance.forbidden` | 403 | Finance | Forbidden financial action |
| `finance.disabled` | 503 | Finance | Financial subsystem is disabled |
| `finance.expenseNotVoidable` | 409 | Finance | Expense is already cancelled or paid and cannot be voided |
| `finance.expenseStateChanged` | 409 | Finance | Expense status changed during the request; retry |
| `finance.reasonRequired` | 400 | Finance | Void reason must be 3 to 500 characters |
| `finance.dateOutOfRange` | 400 | Finance | Expense date is in the future or older than 90 days |
| `request.invalidBody` | 400 | Request | Request payload is malformed or missing required fields |
| `request.invalidId` | 400 | Request | UUID URL parameter is malformed |
| `request.dueDayInvalid` | 400 | Request | Due day must be an integer between 1 and 28 |
| `request.imageTooLarge` | 400 | Request | Uploaded image exceeds maximum allowed size (2MB) |
| `request.imageReadFailed` | 400 | Request | Server failed to read uploaded multipart file stream |
| `preferences.localeRequired` | 400 | Preferences | Locale field is required |
| `preferences.invalidLocale` | 400 | Preferences | Unsupported or invalid locale |
| `kyc.no_consent` | 409 | KYC | No active DPDP consent recorded for tenant |
| `kyc.already_verified` | 409 | KYC | Tenant already has an active, valid KYC verification |
| `kyc.in_flight` | 409 | KYC | Verification attempt is currently in-flight for this tenant |
| `kyc.consent_revoked` | 409 | KYC | Tenant consent was previously revoked under DPDP Rule 8 |
| `kyc.qr_invalid` | 422 | KYC | Aadhaar Secure QR RSA-SHA256 signature verification failed |
| `kyc.qr_incomplete` | 422 | KYC | Aadhaar demographic fields missing (Name, UID last 4) in QR or OCR |
| `kyc.digilocker_unavailable` | 503 | KYC | Cashfree Secure ID service or credentials not configured |
| `kyc.not_found` | 404 | KYC | KYC verification record not found for tenant |
<!-- ERROR_CODES_END -->

### Additional Domain-Specific Error Identifiers (KYC Subsystem)

The KYC identity verification subsystem emits domain-qualified error codes mapped directly to HTTP responses:

| Error Code | HTTP Status | Domain | Trigger Description |
| :--- | :--- | :--- | :--- |
| `kyc.no_consent` | 409 | KYC | No active DPDP consent recorded for tenant before initiation/verification |
| `kyc.already_verified` | 409 | KYC | Tenant already has an active, valid KYC verification |
| `kyc.in_flight` | 409 | KYC | A verification attempt is already in-progress for this tenant (prevents double-billing) |
| `kyc.consent_revoked` | 409 | KYC | Tenant consent was previously revoked under DPDP Rule 8 |
| `kyc.qr_invalid` | 422 | KYC | Aadhaar Secure QR RSA-SHA256 signature verification failed against UIDAI root |
| `kyc.qr_incomplete` | 422 | KYC | Aadhaar demographic fields missing (Name, UID last 4, DOB/YOB) in QR or Smart OCR |
| `kyc.digilocker_unavailable` | 503 | KYC | Cashfree Secure ID service or credentials not configured / upstream unavailable |
| `kyc.not_found` | 404 | KYC | KYC verification record not found for tenant |

## Role Matrix

| Role | JWT Claims | Authorized Route Groups |
| ---- | ---------- | ----------------------- |
| owner | `role=owner`, `property_id` set | `/api/owner/*` (including KYC review & duplicate clear, finance policies, gateway refunds, tenant departures & deductions, operational payouts & batches), `/api/manager/*` (owners may manage facility/inspections), `/api/search`, `/api/notifications`, `/api/me/preferences`, `POST /api/auth/revoke-sessions` |
| manager | `role=manager`, `property_id` set | `/api/manager/*` (mutating `property_id` must match claims), `/api/search`, `/api/notifications`, `/api/me/preferences`, `POST /api/auth/revoke-sessions` |
| tenant (onboarded) | `role=tenant`, `tenant_id` + `property_id`; live status `active` or `pending_allocation` | `/api/tenant/*` (including `/api/tenant/kyc/*`, single & multi-due payment options `/api/tenant/dues/options` & `/api/tenant/dues/pay-batch`), `/api/search`, `/api/notifications`, `/api/me/preferences`, `POST /api/auth/revoke-sessions` |
| pending join | `role=tenant`, `tenant_id` null, `property_id` set | `/api/join/me`, `POST /api/join`, `/api/me/preferences` only. Any `/api/tenant/*` call returns 403 `auth.profileIncomplete` (`"complete your profile to continue"`). |

Never send `property_id` as a *security* claim from the client for owner/tenant scoped lists — owner/tenant money and KYC routes use JWT `property_id`. Some manager/owner **facility** handlers still take `property_id` as query/JSON; managers must match JWT property or get 403 `"manager not authorized for this property"`.

## Auth

Login is Firebase Phone OTP or Google (Gmail). There is **no Owner vs Tenant role picker**. Owners match via seeded `owner_phone` or `owner_email`. Invite code is how a new phone or Google login becomes a pending tenant.

**Primary login is Firebase.** Rent-reminder SMS still uses `SMS_*` / `internal/sms`.

| Method | Path | Rate Limit | Body | Response |
| ------ | ---- | ---------- | ---- | -------- |
| POST | `/api/auth/firebase` | 10 req/min (burst 15) per IP | `{ "id_token": string, "invite_code"?: string }` | `{ "token": string, "user": { "id": uuid, "phone"?: string, "email"?: string, "role": "owner"\|"manager"\|"tenant", "tenant_id"?: uuid, "property_id"?: uuid, "locale": string, "has_saved_preference": boolean, "created_at": string, "last_login_at"?: string } }` |
| POST | `/api/auth/revoke-sessions` | None | (empty) | `{ "ok": true }` — **any authenticated role** (owner, manager, tenant). Increments `users.token_version` so this and all other sessions for that user fail with 403 `"access revoked"` (`auth.accessRevoked`) on the next API call. Client should clear the stored token. |

| Status | When | Error Code |
| ------ | ---- | ---------- |
| 401 | Invalid or claims-less Firebase token (`"authentication failed"`) | `auth.invalidFirebaseToken` |
| 401 | Missing Bearer token in header | `auth.missingToken` |
| 401 | Malformed or cryptographically invalid JWT | `auth.invalidToken` |
| 403 | Tenant vacated | `auth.accessRevoked` |
| 403 | Google email not verified (`"email is not verified with Google"`) | `auth.emailNotVerified` |
| 403 | JWT `token_version` stale | `auth.accessRevoked` |
| 403 | Pending join hitting `/tenant/*` (`"complete your profile to continue"`) | `auth.profileIncomplete` |
| 403 | Manager `property_id` mismatch | `auth.forbidden` |
| 404 | Unknown account without a valid invite | `auth.noAccount` |
| 404 | Invalid invite code provided during exchange | `auth.invalidInvite` |
| 503 | Firebase Admin SDK not configured on server | `auth.firebaseNotConfigured` |

Unknown phone + **valid invite** creates `users.role=tenant` with `tenant_id` null and a `join_requests` row. Until `POST /join` completes, `/tenant/*` returns 403 `auth.profileIncomplete`. After onboarding, the tenant is `pending_allocation` (dashboard allowed; pay disabled until `tenants.status=active`).

### Legacy OTP (rollback / fallback only)

Still registered on the router. Prefer Firebase for primary product login.

| Method | Path | Rate Limit | Body | Response / Notes |
| ------ | ---- | ---------- | ---- | ---------------- |
| POST | `/api/auth/otp/request` | 3 req/min (burst 5) per IP | `{ "phone": string }` | `{ "ok": true }`. Returns 429 `auth.rateLimited` when exceeded. |
| POST | `/api/auth/otp/verify` | None | `{ "phone": string, "otp": string }` | Returns `{ "token", "user": { ... } }` identical to Firebase exchange. |

> **Security Note on Multi-Provider Firebase UIDs and Session Lifetimes:**
> `users.firebase_uid` tracks the user's most-recently authenticated Firebase identity (Phone OTP or Google).
> Revoking a Firebase UID via the Firebase Admin SDK (`auth.RevokeRefreshTokens`) only invalidates Firebase refresh tokens, preventing that identity from minting new Firebase ID tokens to exchange at `POST /auth/firebase`.
>
> - **Tenants:** Revocation is enforced per-request via live database checks in `RequireTenant` (`tenants.status` must be `active` or `pending_allocation`). Marking a tenant `vacated` immediately revokes access on the next API call, regardless of JWT expiry. All roles also carry `token_version` in the JWT, checked against `users.token_version`.
> - **All roles:** `POST /api/auth/revoke-sessions` increments `users.token_version`. Subsequent requests with older JWTs return 403 `auth.accessRevoked`. Firebase UID revocation still only blocks minting new Firebase ID tokens for `POST /auth/firebase`.
>
> **Security Note on Email Verification (Google / Gmail):**
> `POST /auth/firebase` strictly enforces `email_verified: true` in the cryptographically verified Firebase ID token claims for Google logins (returning HTTP 403 `auth.emailNotVerified` if unverified or absent). In Google OIDC federation, this provides cryptographic proof of mailbox possession equivalent in trust to SMS Phone OTP, preventing client-asserted or spoofed email addresses from accessing Owner accounts.

## Join (Invite-in, Dashboard-now)

The invite code **is** authorization. There is no second owner identity review. Owner only assigns room/rent terms later.

| Method | Path | Auth | Payload / Request | Response / Notes |
| ------ | ---- | ---- | ----------------- | ---------------- |
| GET | `/api/join/invite/:code` | Public | None | `{ "property_id": uuid, "property_name": string, "owner_name": string }` — UPI VPA is **never** exposed |
| GET | `/api/join/me` | Pending tenant JWT | None | `{ "join": JoinRequest, "user": { "id": uuid, "role": string, "phone": string, "property_id": uuid }, "message": string }` — only while profile not yet submitted (`tenant_id` null) |
| POST | `/api/join` | Pending tenant JWT | **Multipart** or JSON: `name`, `permanent_address`, `current_address`, `parent_name`, `emergency_phone`, `consent=true`, file `image` (ID photo ≤2MB, no OCR). | Creates `pending_allocation` tenant, links `users.tenant_id`, marks join `approved`. Response: `{ "join": JoinRequest, "tenant": Tenant, "message": string }`. Client must re-exchange Firebase token for a fresh JWT containing `tenant_id`. |
| GET | `/api/owner/invite` | Owner | None | `{ "invite_code": string, "payment_mode": string }` |
| POST | `/api/owner/invite/rotate` | Owner | None | `{ "invite_code": string }` |
| GET | `/api/owner/join-requests` | Owner | Query: `status` (`pending` \| `approved` \| `rejected`) | `{ "join_requests": JoinRequest[] }`. Use `approved` for awaiting room/rent; `pending` for incomplete profiles (rejectable). |
| POST | `/api/owner/join-requests/:id/activate` | Owner | JSON: `{ "room_number"?: string, "rent_amount": int, "due_day": int, "deposit_amount"?: int, "notice_period_days"?: int }` | **Assign terms**: flips tenant to `active`, creates initial deposit due. Returns the updated `Tenant` object directly. |
| POST | `/api/owner/join-requests/:id/reject` | Owner | None | Only while join `status=pending` (tenant abandoned or incomplete). `{ "ok": true }`. |
| GET | `/api/owner/tenants/:id/id-photo` | Owner | None | Raw image bytes (`image/jpeg`, `image/png`, `image/webp`). Never returned in JSON lists (`has_id_photo: boolean` flag only). |

Tenant profile fields: `permanent_address`, `current_address`, `parent_name`, `emergency_phone`, `joined_on` (server date of submit). Personal phone is the login phone.

Walk-in `POST /api/owner/tenants` remains for phone-less or cash-only tenants. Do not use it as the default onboarding path.

Domain Events: `JoinRequested`, `JoinApproved`, `JoinRejected`, `TenantCreated` (pending_allocation), `ConsentGiven` (ID photo), `DepositTermsAccepted` on assign-terms.

## Owner (role=owner)

| Method | Path | Body / Query | Response / Notes |
| ------ | ---- | ------------ | ---------------- |
| GET | `/api/owner/properties` | None | `{ "properties": Property[] }` (includes `id`, `name`, `address?`, `owner_phone`, `owner_name`, `owner_email`, `invite_code?`, `payment_mode?`, `payment_collection_mode?`, `gateway_enabled_at?`, `gateway_sub_merchant_id?`, `created_at`; UPI VPA is **never** returned) |
| GET | `/api/owner/tenants` | None | `{ "tenants": Tenant[] }` — includes profile fields + `has_id_photo`, `aadhaar_last4` |
| POST | `/api/owner/tenants` | `{ "name": string, "phone"?: string, "room_number"?: string, "rent_amount": int, "due_day": int, "notice_period_days"?: int, "deposit_amount"?: int }` | Returns `Tenant` object. Creates walk-in tenant directly. |
| PATCH | `/api/owner/tenants/:id` | `{ "name"?: string, "room_number"?: string, "rent_amount"?: int, "due_day"?: int, "notice_period_days"?: int }` | Partial tenant profile update. Returns updated `Tenant`. |
| GET | `/api/owner/tenants/:id/id-photo` | None | Raw ID photo bytes with correct `Content-Type`. |
| POST | `/api/owner/tenants/:id/notice` | `{ "notice_given_at"?: string }` (optional ISO date) | Sets move-out notice. Returns `{ "ok": true }`. |
| POST | `/api/owner/tenants/:id/vacate` | None | Marks tenant `vacated`. Immediately revokes tenant session on next API call. Returns `{ "ok": true }`. |
| POST | `/api/owner/tenants/:id/attach-phone` | `{ "phone": string }` | Attaches login phone to walk-in tenant. Returns `{ "ok": true }`. |
| POST | `/api/owner/tenants/:id/prorate` | `{ "vacate_date": "YYYY-MM-DD" }` | Computes prorated rent due for move-out month. Returns updated `Due`. |
| POST | `/api/owner/tenants/:id/deposit/settle` | `{ "refunded_amount_paise": int64, "reason"?: string }` | Settles security deposit held. Returns `{ "ok": true }`. |
| GET | `/api/owner/tenants/:id/kyc` | None | Full KYC verification view including `trust_tier`, `method` (`digilocker`\|`ocr`\|`qr`), `qr_status` (`SECURE`\|`PRIMITIVE`\|`NOT_PRESENT`\|`UNPROCESSABLE`), `photo_stored: boolean`, `name_mismatch: boolean`, `duplicate_detected`, `is_dedupable`, and tamper-evident `audit_logs[]`. Property authorization enforced. |
| GET | `/api/owner/tenants/:id/kyc/photo` | None | Owner-only: returns raw image bytes (`image/jpeg`, `image/png`) of attested Aadhaar photo. Returns 404 if missing or revoked. |
| POST | `/api/owner/tenants/:id/kyc/clear-duplicate` | `{ "reason": string }` (required) | Clears false-positive `duplicate_detected` flag with audited human reason. Returns `{ "ok": true }`. |
| GET | `/api/owner/dues` | Query: `tenant_id` (uuid), `kind` (`rent`\|`deposit`\|`electricity`\|`water`), `status` (`pending`\|`partial`\|`paid`\|`waived`) | `{ "dues": Due[] }` |
| POST | `/api/owner/dues/:id/waive` | None | Waives remaining balance of due. Returns updated `Due`. |
| POST | `/api/owner/dues/:id/match` | `{ "amount": int, "upi_txn_id": string }` | Manual match for bank/UPI payment. Returns created `Payment`. |
| POST | `/api/owner/dues/:id/mark-cash-paid` | `{ "amount": int, "note"?: string }` | All-or-nothing cash settlement. Returns created `Payment`. |
| POST | `/api/owner/dues/bulk-mark-paid/preview` | `{ "due_ids": uuid[] }` | Computes eligible dues and total paise, filtering settled/foreign dues. Returns `{ "eligible_count": int, "total_amount_paise": int64, "total_amount_rupees": float, "eligible_dues": EligibleDueItem[], "skipped_dues": SkippedDueItem[] }`. |
| POST | `/api/owner/dues/bulk-mark-paid/confirm` | `{ "due_ids": uuid[], "confirmed_total_paise": int64, "note"?: string }` | Verifies re-entered total paise matches eligible sum, then atomically marks all cash paid under a shared batch reference. Returns `{ "settled_count": int, "total_amount_paise": int64, "batch_ref": string, "payments": Payment[] }`. |
| GET | `/api/owner/dues/:id/qr` | None | Dynamic PNG QR image bytes. Returns 409 in Cashfree mode. |
| GET | `/api/owner/dues/:id/pay` | None | JSON pay payload (see Pay JSON schema). |
| POST | `/api/owner/dues/:id/token` | None | Creates 72-hour magic payment token. Returns `{ "path": string, "url": string, "wa_me": string }`. |
| GET | `/api/owner/occupancy` | None | Aggregated bed capacity, occupancy counts, vacant beds, and floor/room vacancy breakdown. Returns `{ "occupancy_rate_bps": int, "total_capacity": int, "occupied_beds": int, "vacant_beds": int, "floors": FloorOccupancy[] }`. |
| GET | `/api/owner/dashboard/summary` | Query: `period=YYYY-MM` (optional, defaults to current month in Asia/Kolkata) | Single-request owner dashboard summary. Returns `{ "property_id": uuid, "period": string, "total_occupancy": PropertyOccupancySummary, "floor_occupancy": FloorOccupancy[], "total_collected_paise": int64, "total_pending_paise": int64, "total_expense_paise": int64, "net_cash_flow_paise": int64, "collection_percentage": float64, "monthly_cash_flow": MonthlyCashFlowItem[], "as_of": timestamp }`. |
| GET | `/api/owner/dashboard/monthly-cashflow` | Query: `months=int` (default 6, max 36) | Server-side periodic monthly cash flow aggregation for owner charts. Returns `{ "property_id": uuid, "cash_flow": MonthlyCashFlowItem[] }`. |
| GET | `/api/owner/calendar/token` | None | Generates HMAC-signed iCal calendar feed subscription URL. Returns `{ "property_id": uuid, "token": string, "calendar_url": string }`. |
| GET | `/api/owner/settings` | None | Returns active property settings `{ "settings": PropertySettings }`. |
| PATCH | `/api/owner/settings` | `{ "payout_auto_dispatch"?: bool, "reminder_offsets"?: int[], "reminder_catch_up_days"?: int, "active_modules"?: map[string]bool, "auto_apply_credit"?: bool }` | Updates property settings. Returns `{ "settings": PropertySettings }`. |
| GET | `/api/owner/payment-reports` | Query: `status` (`pending_review`\|`confirmed`\|`rejected`) | `{ "payment_reports": PaymentReport[] }` (includes `image_hash`, `is_duplicate`, `ocr_*` fields). |
| POST | `/api/owner/payment-reports/:id/confirm` | None | Confirms reported payment → executes `ManualMatch`. Returns `{ "report": PaymentReport, "payment": Payment }`. |
| POST | `/api/owner/payment-reports/:id/reject` | `{ "note"?: string }` (optional) | Rejects payment report with reason note. Returns `{ "report": PaymentReport }`. |
| GET | `/api/owner/payments` | Query: `matched_by` (`due_code`\|`csv_auto`\|`manual`\|`cash`\|`cashfree`), `limit` (max 50), `before` (timestamp cursor) | `{ "payments": Payment[] }` (paginated, max 50 per request). |
| POST | `/api/owner/payments/verify` | `{ "due_id": uuid, "amount_paise": int, "upi_txn_id": string, "note"?: string }` | Atomic, idempotent server-side payment verification. Validates positive integer paise amount, normalizes UTR (trims, uppercase), validates format, enforces UTR uniqueness, and updates due atomically. Returns `Payment`. |
| POST | `/api/owner/payments/:id/correct` | `{ "corrected_amount_paise": int, "corrected_upi_txn_id"?: string, "reason": string }` | Creates immutable financial correction: posts reversing entry, posts corrected entry, adjusts due balance, and logs immutable entry in `financial_corrections`. Returns `FinancialCorrection`. |
| POST | `/api/owner/payments/:id/refund` | `{ "amount_paise": int64, "reason": string }` (optional header `X-Idempotency-Key`) | Initiates owner gateway refund on Cashfree payment under universal lock hierarchy `{1,3,6,7,8}`. Allocates refund across dues via LIFO order (`refund_allocations`). Blocks with 400 if exceeding refundable balance, non-Cashfree payment, payment >180 days old, or Guard A (tenant has settled departure `approved`/`refunded` or dues have `departure_due_adjustments`). Zero locks held during Cashfree network call. Returns `GatewayRefund` object. |
| POST | `/api/owner/statements/import` | Multipart: `file` (CSV, max 5MB; required columns: `txn_id`, `amount`, `date`, `note`) | Reconciles bank statement. Returns `{ "row_count": int, "matched": int, "failed": int }`. |
| GET | `/api/owner/reconciliation` | Query: `period=YYYY-MM` (defaults to current month) | Returns `ReconciliationSummary` object. |
| GET | `/api/owner/events` | Query: `tenant_id`, `type`, `from`, `to` (`YYYY-MM-DD`), `limit` (max 50), `offset` | `{ "events": Event[] }` (internal bigserial IDs redacted, max 50 records per request). |

### Tenant Departures & Deductions (Owner, ADR-009)

| Method | Path | Body / Query | Response / Notes |
| ------ | ---- | ------------ | ---------------- |
| POST | `/api/owner/tenants/:id/departures` | `{ "planned_vacate_date": "YYYY-MM-DD", "deposit_amount_paise"?: int64, "notes"?: string }` | Records move-out notice for active tenant (status `pending`). Defaults deposit to 1 month rent if omitted. Returns 201 `{ "departure": TenantDeparture }`. Returns 409 if active departure already exists (`uq_tenant_one_active_departure`). |
| POST | `/api/owner/departures/:id/inspect` | `{ "inspected_at"?: string, "notes"?: string }` | Records physical inspection and key handover. Starts 24-hour settlement SLA (`sla_deadline_at = inspected_at + 24h`). Sets status `inspected`. Returns 200 `{ "status": "inspected", "inspected_at": string, "sla_deadline_at": string }`. |
| POST | `/api/owner/departures/:id/deductions` | `{ "description": string, "amount_paise": int64, "evidence_photo_key"?: string, "status"?: "agreed"\|"disputed"\|"waived" }` | Records itemized damage or utility deduction against departure. Amount must be positive. Returns 201 `{ "deduction": DepartureDeduction }`. |
| POST | `/api/owner/departures/:id/settle` | `{ "actual_vacate_date": "YYYY-MM-DD", "prorated_rent_paise"?: int64, "payee_id"?: uuid, "notes"?: string }` | Executes `SettleDepartureUnderLock` adhering to ADR-008 topological lock hierarchy `{1,3,6,7,10,11,12,13,15}`. Atomically nets rent/open dues, locks cycle rent due with `contractual_ceiling_paise`, inserts `departure_due_adjustments` for prepaid excess, marks tenant `vacated`, creates `payout_items` if `net_refund_paise > 0`, records `receivable_balance_paise` if negative, and posts balanced mirror journal. Returns 200 `{ "departure": TenantDeparture, "payout_item"?: PayoutItem, "due_adjustments"?: DepartureDueAdjustment[], "net_refund_paise": int64, "receivable_balance_paise": int64, ... }`. |

### Operational Payouts Subsystem (Owner, ADR-009)

| Method | Path | Body / Query | Response / Notes |
| ------ | ---- | ------------ | ---------------- |
| POST | `/api/owner/payouts/payees` | `{ "payee_type": "staff"\|"vendor"\|"tenant_deposit"\|"guardian_deposit", "name": string, "phone"?: string, "account_number"?: string, "ifsc"?: string, "bank_name"?: string, "upi_vpa"?: string }` | Provisions payout destination. Validates either `account_number` or `upi_vpa`. Computes HMAC-SHA256 account hash (`account_number_hash`) using property secret. Returns 201 `{ "payee": PayoutPayee }`. |
| GET | `/api/owner/payouts/payees` | None | Lists verified payout payees scoped to owner property. Returns `{ "payees": PayoutPayee[] }`. |
| GET | `/api/owner/payouts/items/unbatched` | None | Lists unbatched payout items (`status = 'pending'`, `batch_id = NULL`) awaiting batch packaging. Returns `{ "items": PayoutItem[] }`. |
| POST | `/api/owner/payouts/batches` | `{ "notes"?: string }` | Atomically packages all pending unbatched payout items into an immutable batch with deterministic `batch_number` (`BATCH-YYYYMMDD-<hex>`) and HMAC-SHA256 checksum (`file_checksum`). Returns 201 `{ "batch": PayoutBatch, "items": PayoutItem[] }`. |
| POST | `/api/owner/payouts/batches/:id/approve` | `{ "expected_item_count": int, "expected_total_paise": int64, "step_up_otp"?: string, "firebase_id_token"?: string }` | Dual-control maker-checker approval. On multi-owner properties enforces `claims.UserID != batch.CreatedBy`; on solo-owner properties requires cryptographic step-up reauthentication. Returns `{ "batch": PayoutBatch }`. |
| POST | `/api/owner/payouts/batches/:id/approve/request-otp` | None | Dispatches purpose-parameterized SMS OTP to owner phone for batch approval step-up reauthentication. Returns `{ "ok": true }`. |
| GET | `/api/owner/payouts/batches/:id/export` | None | Exports batch instruction sheet as CSV download. Sets headers `Content-Type: text/csv`, `Content-Disposition: attachment; filename="payout_<batch_number>.csv"`, and `X-Batch-Checksum: <hmac-sha256>`. |
| POST | `/api/owner/payouts/batches/:id/dispatch` | `{ "mode": "cashfree" }` | Dispatches approved batch to Cashfree Transfers V2 API. Idempotent per batch. Returns `{ "batch": PayoutBatch, "dispatched_count": int, "results": ... }`. |

### Bank Accounts & Transaction Reconciliation (Owner, Track O)

| Method | Path | Body / Query | Response / Notes |
| ------ | ---- | ------------ | ---------------- |
| GET | `/api/owner/bank-accounts` | None | Lists active bank accounts for property. Returns `{ "bank_accounts": BankAccount[] }`. |
| POST | `/api/owner/bank-accounts` | `{ "bank_name": string, "account_type"?: "savings"\|"current", "account_number_last4": string, "label"?: string, "statement_profile"?: string }` | Creates bank account tracking record. Returns 201 `BankAccount`. |
| DELETE | `/api/owner/bank-accounts/:id` | None | Soft-deactivates bank account. Returns 200 `{ "status": "deactivated" }`. |
| GET | `/api/owner/statements/transactions` | Query: `account_id` (uuid), `status` (`unmatched`\|`matched`\|`refunded`\|`classified`), `from`, `to` (`YYYY-MM-DD`) | Lists ingested bank statement transactions. Returns `{ "transactions": BankTransaction[] }`. |
| POST | `/api/owner/statements/transactions/:id/confirm` | `{ "target_due_id"?: uuid }` | Confirms match between bank transaction and due (Tier 2 heuristic promotion to settled payment with ledger mirror). Returns `{ "status": "matched", "transaction": BankTransaction }`. |
| POST | `/api/owner/statements/transactions/:id/refund` | None | Marks unidentified deposit refunded back to sender. Returns `{ "transaction": BankTransaction }`. |
| POST | `/api/owner/statements/transactions/:id/classify` | `{ "classification": "capital_injection"\|"vendor_refund"\|"other_income", "notes"?: string }` | Classifies non-rent bank transaction into appropriate general ledger category. Returns `{ "transaction": BankTransaction }`. |

### Gateway Settlements & EOD Balancer (Owner, Tracks N & P)

| Method | Path | Body / Query | Response / Notes |
| ------ | ---- | ------------ | ---------------- |
| GET | `/api/owner/settlements` | Query: `status` (`pending`\|`reconciled`\|`discrepant`), `limit` | Lists Cashfree gateway settlement records. Returns `{ "settlements": SettlementRecord[] }`. |
| GET | `/api/owner/settlements/:id` | None | Retrieves detailed settlement record with order-to-intent reconciliation items. Returns `SettlementRecord`. |
| POST | `/api/owner/settlements/:id/resolve` | `{ "resolution_action": "accept_variance"\|"manual_adjust", "notes": string, "step_up_otp"?: string }` | Human-gated maker-checker discrepancy resolution for gateway settlement differences. Returns `{ "settlement": SettlementRecord }`. |
| GET | `/api/owner/settlements/eod-balance` | Query: `date=YYYY-MM-DD` (defaults to today) | Fetches Multi-Way End-of-Day balance snapshot reconciling gateway in-transit, bank cleared, unapplied receipts, and general ledger. Returns `DailySettlementBalance`. |
| POST | `/api/owner/settlements/eod-balance/run` | `{ "date": "YYYY-MM-DD" }` | Computes and persists durable daily settlement balance snapshot. Returns 200/201 `DailySettlementBalance`. |
| GET | `/api/owner/settlements/eod-balance/history` | Query: `from`, `to` (`YYYY-MM-DD`) | Lists historical daily settlement balance snapshots for trend and variance audit. Returns `{ "history": DailySettlementBalance[] }`. |

### Staff Attendance & Payroll Subsystem (Owner, Track M)

| Method | Path | Body / Query | Response / Notes |
| ------ | ---- | ------------ | ---------------- |
| POST | `/api/owner/staff` | `{ "payee_id": uuid, "name": string, "role": string, "phone"?: string, "base_monthly_wage_paise": int64, "effective_from": "YYYY-MM-DD" }` | Provisions staff profile linked to verified property payee. Returns 201 `StaffProfile`. |
| GET | `/api/owner/staff` | Query: `active_only` (bool, default true) | Lists staff profiles for property. Returns `{ "staff": StaffProfile[] }`. |
| PUT | `/api/owner/staff/:id/status` | `{ "status": "active"\|"inactive", "effective_to"?: "YYYY-MM-DD" }` | Updates staff active status and end-of-employment date. Returns `{ "message": "staff status updated" }`. |
| GET | `/api/owner/attendance/leave-policy` | None | Retrieves property leave policy and paid holidays. Returns `LeavePolicy`. |
| PUT | `/api/owner/attendance/leave-policy` | `{ "monthly_free_leave_days": int, "paid_holidays": string[], "working_days_basis": "calendar_days"\|"fixed_30"\|"working_days_excluding_sundays" }` | Upserts property leave policy rules. Returns updated `LeavePolicy`. |
| POST | `/api/owner/attendance/daily` | `{ "work_date": "YYYY-MM-DD", "entries": [{ "staff_id": uuid, "status": "present"\|"absent"\|"paid_leave"\|"half_day"\|"holiday", "notes"?: string }] }` | Records daily bulk attendance check-in for staff. Returns `{ "message": "attendance recorded successfully" }`. |
| GET | `/api/owner/attendance/monthly` | Query: `month=YYYY-MM` (defaults to current month) | Lists all daily attendance check-in records for given month. Returns `{ "month": string, "records": AttendanceRecord[] }`. |
| POST | `/api/owner/payroll/calculate` | `{ "cycle_month": "YYYY-MM" }` | Previews monthly wage calculations and proration for all active staff without persisting. Returns `{ "cycle_month": string, "calculations": WageCalculation[] }`. |
| POST | `/api/owner/payroll/finalize` | `{ "cycle_month": "YYYY-MM" }` | Atomically calculates, freezes immutable monthly wage snapshots, and injects unbatched `payout_items` (`PayeeTypeStaff`) into the payout pipeline. Returns `{ "cycle_month": string, "finalized": WageCalculation[] }`. |


### Facility & Gamification (Owner)

| Method | Path | Body / Query | Response / Notes |
| ------ | ---- | ------------ | ---------------- |
| GET | `/api/owner/gamification/settings` | Query: `property_id` (required) | `{ "settings": PropertyGamificationSettings, "rules": GamificationRule[] }` |
| PATCH | `/api/owner/gamification/settings` | JSON: `PropertyGamificationSettings` (must include `property_id`) | `{ "settings": PropertyGamificationSettings }` |
| GET | `/api/owner/floors` | Query: `property_id` (required) | `{ "floors": Floor[] }` |
| POST | `/api/owner/floors` | `{ "property_id": uuid, "floor_number": int, "name": string }` | `{ "floor": Floor }` |
| GET | `/api/owner/rooms` | Query: `property_id` (required) | `{ "rooms": Room[] }` |
| POST | `/api/owner/rooms` | `{ "property_id": uuid, "floor_id": uuid, "room_number": string, "capacity": int, "included_units"?: int }` | `{ "room": Room }` |
| POST | `/api/owner/managers` | `{ "phone": string, "property_id"?: uuid }` | Provisions `users.role=manager`. Defaults to claims `property_id`. Returns `{ "manager": User }`. |

#### PropertyGamificationSettings Shape
```json
{
  "property_id": "31b6ea55-ec44-42b7-a3f1-f896b5fc24ee",
  "point_value_paise": 100,
  "monthly_budget_paise": 1000000,
  "reward_budget_basis_points": 150,
  "reward_budget_ceiling_basis_points": 200,
  "earn_cap_per_tenant": 100,
  "rsvp_sub_cap": 60,
  "expiry_days": 180,
  "floor_bonus_threshold": 85,
  "electricity_tariff_paise": 1000,
  "grace_days": 2,
  "late_penalty_points_per_day": 2,
  "late_penalty_max_points": 50,
  "created_at": "2026-09-01T00:00:00Z",
  "updated_at": "2026-10-08T00:00:00Z"
}
```
*Note on F7 Economics:*
- `reward_budget_basis_points` defaults to `150` (1.50% of monthly rent roll pool).
- `reward_budget_ceiling_basis_points` defaults to `200` (2.00% hard ceiling of monthly rent roll).
- `earn_cap_per_tenant` defaults to `100` points per tenant per calendar month.
- Effective monthly point budget = `min(rent_roll * basis_points / 10000, rent_roll * ceiling_basis_points / 10000) / point_value_paise`. If rent roll is 0, falls back to `monthly_budget_paise / point_value_paise`.

### ReconciliationSummary Shape

```json
{
  "period": "2026-08",
  "rent_collected_paise": 0,
  "by_channel": { "due_code": 0, "cash": 0 },
  "outstanding_paise": 0,
  "credits_held_paise": 0,
  "deposits_held_paise": 0,
  "deposits_refunded_paise": 0
}
```

## Manager / Warden (`RequireManagerOrOwner`)

Owners may call all manager routes. Managers must operate on their JWT property (`property_id` mismatch returns 403 `auth.forbidden`).

| Method | Path | Body / Query | Response / Notes |
| ------ | ---- | ------------ | ---------------- |
| POST | `/api/manager/inspections` | `{ "property_id": uuid, "room_id"?: uuid, "floor_id"?: uuid, "inspection_type": string, "notes": string, "items": [ { "item_key": string, "description": string, "passed": boolean, "photo_base64"?: string, "notes"?: string } ] }` | Submits property inspection. Returns `{ "inspection": Inspection }`. |
| GET | `/api/manager/inspections` | Query: `property_id` (defaults from JWT) | `{ "inspections": Inspection[] }` (includes inspection items). |
| POST | `/api/manager/inspections/items/:id/resolve` | `{ "status": "upheld" \| "overturned" }` | Resolves tenant inspection dispute. Returns `{ "status": "resolved", "resolution": "upheld" \| "overturned" }`. |
| POST | `/api/manager/violations` | `{ "tenant_id": uuid, "rule_code": string, "severity": "safety" \| "lifestyle", "description": string, "evidence_base64"?: string }` | Logs tenant discipline violation. Returns `{ "violation": Violation }`. |
| POST | `/api/manager/meter-readings` | `{ "property_id": uuid, "kind": "electricity" \| "water", "reading_value": float64, "room_id"?: uuid, "floor_id"?: uuid, "meter_replaced"?: boolean, "confirm_anomaly"?: boolean }` | Records utility meter reading. Returns `{ "result": MeterReadingResult }`. |
| GET | `/api/manager/kitchen/headcount` | Query: `property_id` (defaults from JWT), `date?` (`YYYY-MM-DD`) | Returns `{ "headcount": KitchenHeadcount }`. |
| GET | `/api/manager/hazards` | Query: `property_id` (defaults from JWT), `status?` | `{ "hazards": Hazard[] }`. |
| POST | `/api/manager/hazards/:id/resolve` | `{ "status": "resolved" \| "rejected" }` (defaults to `resolved` if omitted) | Marks hazard resolved or rejected. Returns `{ "status": "resolved" \| "rejected" }`. |
| POST | `/api/manager/vendor-inspections` | `{ "property_id": uuid, "vendor_name": string, "score_percent": int, "notes"?: string, "penalty_paise"?: int64, "photo_base64"?: string }` | Records vendor audit score. Returns `{ "vendor_inspection": VendorInspection }`. |
| GET | `/api/manager/finance/expenses` | None | `{ "expenses": Expense[] }`. Scoped to manager property. |
| POST | `/api/manager/finance/expenses` | `{ "category_code": string, "vendor_name": string, "description": string, "amount_paise": int64, "emergency"?: boolean, "room_id"?: uuid }` | Header: `Idempotency-Key`. Checked against `approval_policies` in DB. **201** `{ "expense": Expense, "approval"?: ApprovalRequest }`. |
| POST | `/api/manager/finance/expenses/:id/payments` | `{ "amount_paise": int64, "method": "cash" \| "upi" \| "bank" }` | Header: `Idempotency-Key`. Out-of-pocket manager payments recorded as `manager_advances`. **201** `{ "payment": ExpensePayment }`. |
| GET | `/api/manager/finance/today` | None | Operational finance dashboard: `{ "expenses": Expense[], "pending_approvals": ApprovalRequest[], "kitchen_headcount": KitchenHeadcount, "hide_capital": boolean }`. `hide_capital` is `true` when caller is a manager **and** `ManagerCanViewCapital` is `false` in property finance settings. |
| POST | `/api/manager/finance/meal-prep` | `{ "meal_date": "YYYY-MM-DD", "meal_slot": "breakfast" \| "lunch" \| "dinner", "prepared_count": int, "discarded_count"?: int }` | Actuals for food leakage detector. Returns `{ "prep": MealPrepActual }`. |

## Tenant (`active` or `pending_allocation`)

`RequireTenant` allows `active` and `pending_allocation`. **Pay / QR / Cashfree checkout / UTR reports** require `tenants.status=active` (`IsPayable()`). Pending-allocation tenants can load dashboard, KYC onboarding, points, community, meal RSVP, and profile.

### Ledger & Payment

| Method | Path | Auth | Notes |
| ------ | ---- | ---- | ----- |
| GET | `/api/tenant/me` | Tenant JWT | Tenant object + `active_dues: Due[]`. |
| GET | `/api/tenant/dues` | Tenant JWT | `{ "dues": Due[] }`. |
| GET | `/api/tenant/dues/options` | Tenant JWT | Computes FIFO aggregated payment options based on open dues count: `1` (oldest due alone), `2` (oldest 2 dues together), and `all` (clear all dues). Returns 200 `{ "total_outstanding_paise": int, "options": PaymentOption[] }`. |
| POST | `/api/tenant/dues/pay-batch` | Tenant JWT | `{ "option_type"?: "1"\|"2"\|"all", "due_ids"?: uuid[] }`. Generates or reuses a multi-due `payment_intent` covering the selected dues with Cashfree `payment_session_id`. Enforced by partial unique index `uq_active_intent_dues_item` and sync trigger. Returns 200 `domain.PayIntent`. |
| GET | `/api/tenant/dues/:id/qr` | Tenant JWT | PNG image (own due only); returns 409 in Cashfree mode. |
| GET | `/api/tenant/dues/:id/pay` | Tenant JWT | JSON pay payload (see Pay JSON schema). `payable` is false if due is paid/waived **or** tenant is not `active`. |
| POST | `/api/tenant/dues/:id/reports` | Tenant JWT | **Multipart** (`image` ≤2MB, `upi_txn_id`, `amount?`, `note?`) or JSON (`{ upi_txn_id, amount?, note? }`). Computes SHA-256 `image_hash`, sets `is_duplicate = true` if duplicate image detected in property. Returns 201 `{ "id": uuid, "due_id": uuid, "tenant_id": uuid, "property_id": uuid, "upi_txn_id": string, "amount": int, "has_image": boolean, "status": "pending_review", "reported_by": uuid, "note"?: string, "created_at": string }`. Note: `is_duplicate` and `image_hash` are strictly withheld from the tenant and surfaced only to the owner. |
| GET | `/api/tenant/payments` | Tenant JWT | `{ "payments": Payment[] }`. |
| POST | `/api/tenant/push/subscribe` | Tenant JWT | `{ "endpoint": string, "keys": { "p256dh": string, "auth": string } }` → `{ "ok": true }`. |
| POST | `/api/tenant/aadhaar` | Tenant JWT | Legacy Secure QR / Last-4 submission: `{ "consent": true, "qr_payload"?: string, "name"?: string, "dob"?: string, "gender"?: string, "uid_last4"?: string, "confirm"?: boolean }`. Without `confirm`, returns decoded preview. |

### Tenant Identity Verification (ADR-004 KYC)

Production Aadhaar identity verification compliant with DPDP Act 2023. Gated by active consent and rate-limited per tenant ID.

#### Architecture & Invariants

1. **Two-Tier Primary Flow**:
   - **Primary (DigiLocker)**: Live government-attested consent flow via Cashfree Secure ID (`/verification/digilocker`). High assurance, tamper-proof demographic data and photo fetched directly from DigiLocker/UIDAI.
   - **Presented Fallback (Smart OCR & Embedded QR)**: Single document upload pipeline for tenants who cannot or decline to use DigiLocker (`POST /api/tenant/kyc/upload`). Powered by Cashfree Smart OCR (`POST /bharat-ocr` with `document_type=AADHAAR`), accepting a single file (JPEG, JPG, PNG, or PDF up to 5MB). Cashfree server-side automatically executes OCR, fraud checks, and reads embedded QR codes if present (populating `qr_details.status`). The consumer PWA presents only this file upload fallback.
   - **Headless Fast-Path (`POST /api/tenant/kyc/qr`)**: An auxiliary, zero-API-cost route for direct raw QR string payloads decoded by client hardware or admin/warden scanning tools. Authenticated under `auth.RequireTenant`. Validated in-process via UIDAI 2048-bit RSA PKCS#1 v1.5 SHA-256 digital signature (`internal/aadhaar`). When verified via this route, the backend records `method='qr'` and sets `qr_status='SECURE'`.
2. **Onboarding Lifecycle & Concurrency Invariants**:
   - **Non-Blocking Onboarding**: KYC identity verification does not hard-block tenant login or dashboard access (`pending_allocation` role allowed). It surfaces as an assurance trust badge to property owners to inform room/rent term activation (`POST /api/owner/join-requests/:id/activate`).
   - **Atomic In-Flight Supersession**: Only one verification row may be active (`pending` or `verified`) per tenant, enforced by partial unique index `idx_kyc_one_active_per_tenant`. If a tenant starts DigiLocker and subsequently switches to Document Upload, `InitiateVerificationTx` acquires a `FOR UPDATE` tenant row lock and atomically marks the existing `pending` DigiLocker row as `failed` (`failure_reason = "superseded by new attempt"`).
   - **TOCTOU Webhook Protection**: If a late DigiLocker webhook or reconciliation poll arrives for a superseded session, `CompleteVerificationTx` verifies that the row is still `pending`. If not, it safely returns `nil` (idempotent no-op), preventing dual in-flight race conditions.
   - **Retry After Failure / Expiry**: When a verification transitions to `failed` or `expired` (e.g. `consent_window_expired` after 60 min), it ceases to be active under the partial unique index. The tenant is immediately permitted to re-attempt verification via either DigiLocker or Document Upload.
3. **Document Upload Requirements**:
   - Identity verification strictly requires demographic fields (Name, Date/Year of Birth, Gender, Masked UID) to bind the tenant and compute `identity_hash`.
   - Single-file submission (`file`) accepts either an **e-Aadhaar PDF** (which contains full details) or an **Aadhaar Front card image** (or full card scan).
   - If a tenant uploads an Aadhaar Back image alone (`AADHAAR_BACK`) that lacks name/DOB, the endpoint rejects the request with HTTP 422 `kyc.ocr_failed` ("demographic details missing; front side or full e-Aadhaar PDF required").
4. **Identity Hash & Key Rotation Specification**:
   - **Purpose**: Detect multi-account registration and duplicate tenant fraud platform-wide across all properties without persisting raw 12-digit UID numbers.
   - **Inputs**: Unicode NFC normalized lowercase full name (`NormalizeName`), canonical ISO `YYYY-MM-DD` DOB (or `YYYY-01-01` if year only), uppercase gender (`M`, `F`, `T`), and masked UID last-4 digits (`masked_uid`).
   - **Algorithm**: `HMAC-SHA256(IDENTITY_SECRET, "name|dob|gender|masked_uid")`.
   - **Key Versioning**: Tracked via `hash_key_version SMALLINT NOT NULL DEFAULT 1` on `kyc_verification`. If `IDENTITY_SECRET` is rotated in the future, new records use bumped key versions while historical records remain tied to their version.
   - **Privacy Invariant**: The raw UID is never persisted or hashed. The raw HMAC is never returned in client JSON responses (`duplicate_detected: boolean` flag only).
   - **Index & Collision Behavior**: Backed by partial unique index `idx_kyc_dedup_hash` on `kyc_verification(identity_hash)` where `status = 'verified'` AND `is_dedupable = TRUE`. Detection flags `duplicate_detected = TRUE` without blocking completion; owners can review and clear false positives via `POST /api/owner/tenants/:id/kyc/clear-duplicate`.
5. **Network-Before-Transaction (ADR-004)**: Upstream network calls to Cashfree (DigiLocker session creation, Smart OCR upload, document fetch) are executed **before** opening DB write transactions to prevent connection pool exhaustion.
6. **Reconciliation & 1-Hour Consent Expiry**:
   - A tenant verification session remains `status='pending'` in `pg-go` until the document is successfully fetched or permanently fails.
   - Cashfree holds user consent for **60 minutes** before closing the document-fetch window.
   - The reconciliation sweep (`cmd/digilocker-reconcile`) runs every **5 minutes** to catch any dropped webhooks or transient failures, polling Cashfree's session status:
     - For rows aged **5 to 60 minutes**: active polling against Cashfree `GetDigiLockerStatus`. If `COMPLETED`/`AUTHENTICATED`, invokes `ProcessDigiLockerCompletion` to fetch document and mark `verified`. If Cashfree reports `FAILED`, marks `failed`.
     - For rows older than **60 minutes** still in `pending`: force-transitions to `failed` with reason `consent_window_expired`, eliminating dangling rows.
7. **Photo Provenance, Security & DPDP Act 2023 Compliance**:
   - **Separation of Tenant Join Photo vs. KYC Attested Photo**:
     - `tenants.id_photo_bytes` (`BYTEA`) holds the tenant's self-submitted photo uploaded during initial join onboarding (`POST /join`). This operational profile photo is **never overwritten** by KYC.
     - `kyc_verification.attested_photo_bytes` (`BYTEA`) holds the government/vendor-attested photo extracted from DigiLocker or Smart OCR (`POST /api/tenant/kyc/upload`).
     - `kyc_verification.photo_stored` (`BOOLEAN`) records whether `attested_photo_bytes IS NOT NULL`.
   - **Zero Clobbering & Name Mismatch Invariant**:
     - `CompleteVerificationTx` updates **only** `tenants.aadhaar_last4 = maskedUID`. It does **not** overwrite `tenants.name` or `tenants.id_photo_bytes`.
     - At verification completion, the service evaluates name similarity (`domain.IsNameMatch`) between self-reported `tenants.name` and attested `doc.Name`. If a mismatch is detected, `name_mismatch = TRUE` is flagged on `kyc_verification` for owner review.
     - To comply with DPDP Rule 8, `kyc_audit_log` records `action = 'name_mismatch'` with structured non-PII token `detail = 'mismatch_detected=true'`. **Raw PII name strings are never embedded in the immutable audit log.**
   - **Owner Photo Access**:
     - `GET /api/owner/tenants/:id/id-photo`: returns the tenant's self-submitted join photo.
     - `GET /api/owner/tenants/:id/kyc/photo`: returns the government/vendor-attested Aadhaar photo.
   - **Atomic DPDP Rule 8 Erasure**: On consent revocation (`POST /api/tenant/kyc/revoke`), `RevokeConsent` executes an atomic multi-table wipe within a single transaction under an exclusive tenant row lock: zeroes `masked_uid`, `identity_hash`, and `attested_photo_bytes` across `kyc_verification`, resets `photo_stored = FALSE`, zeroes `tenants.aadhaar_last4`, and appends an immutable zero-PII audit log entry. The tenant's operational join profile remains intact while all Aadhaar PII is completely erased.

#### Route Summary

| Method | Path | Rate Limit | Request Body | Response / Notes |
| ------ | ---- | ---------- | ------------ | ---------------- |
| POST | `/api/tenant/kyc/consent` | None | `{ "purpose": string, "consent_version": string, "consent_text": string }` | Records DPDP consent before any verification is permitted. Stores client IP and User-Agent. Returns 201 `{ "id": uuid, "consent_version": string, "consent_given_at": string }`. |
| POST | `/api/tenant/kyc/initiate` | 10 req/min (burst 2) per `tenant_id` | (empty) | Initiates DigiLocker verification via Cashfree Secure ID. Pre-checks verification status and enforces 30s distributed lease via `kyc_in_flight_lock` (rejects concurrent in-flight submissions with 409 `kyc.in_flight`). Generates upstream session link *before* DB row insertion (network-before-transaction). Returns 200 `{ "verification_url": string }`. |
| GET | `/api/tenant/kyc/return` | 10 req/min (burst 3) per `tenant_id` | Query: `vendor_ref_id=<uuid>` | Browser-return redirect poller: lightweight check when tenant is redirected back from DigiLocker. Reads local DB row status directly without triggering duplicate upstream doc fetches. Returns 200 `TenantKYCView`. |
| POST | `/api/tenant/kyc/upload` | 5 req/min (burst 2) per `tenant_id` | **Multipart**: `file` (JPEG, JPG, PNG, or PDF ≤ 5MB) | Primary document upload fallback: submits file to Cashfree Smart OCR (`POST /bharat-ocr`, `document_type=AADHAAR`). Enforces 30s distributed lease via `kyc_in_flight_lock` (rejects concurrent in-flight submissions with 409 `kyc.in_flight`). Runs two-step `InitiateVerificationTx` (superseding pending sessions) → `CompleteVerificationTx`. Stores `qr_status`, `name_mismatch`, `attested_photo_bytes`, checks duplicates, and marks verified. Returns 200 `TenantKYCView`. |
| POST | `/api/tenant/kyc/qr` | None | `{ "raw_qr": string }` | Headless fast-path: decodes and verifies UIDAI Secure QR string using 2048-bit RSA PKCS#1 v1.5 SHA-256 signature against UIDAI public cert. Runs two-step initiate→complete. Sets `method='qr'`, `qr_status='SECURE'`. Returns 200 `TenantKYCView`. |
| POST | `/api/tenant/kyc/revoke` | None | (empty) | DPDP Rule 8 compliance: immediately revokes consent, nulls `masked_uid`, `identity_hash`, `attested_photo_bytes`, and `tenants.aadhaar_last4`. Retains compliance skeleton. Returns 200 `{ "ok": true, "message": "consent revoked and PII scrubbed" }`. |
| GET | `/api/tenant/kyc/status` | None | None | Returns `{ "has_consent": boolean, "has_verification": boolean, "consent_version"?: string, "consent_given_at"?: string, "verification"?: TenantKYCView }`. Fraud signals (`identity_hash`, `duplicate_detected`, `vendor_reference_id`, `name_mismatch`) are stripped. |
| GET | `/api/owner/tenants/:id/kyc/photo` | None | None | Owner-only: returns the binary `image/jpeg` or `image/png` of the KYC-attested Aadhaar photo (`kyc_verification.attested_photo_bytes`). Returns 404 if no attested photo stored or revoked. |

#### Trust-Tier Matrix & UI Badging

The owner dashboard displays a contextual verification badge reflecting the exact cryptographic and documentary assurance level.
**Status Invariant**: Trust tiers only evaluate for verified records (`status = 'verified'`). If `status != 'verified'`, the verification returns `trust_tier = 'unverified'`.

| Trust Tier Code | Display Badge | Precedence & Condition (when `status = 'verified'`) | Assurance Level |
| :--- | :--- | :--- | :--- |
| `unverified` | **Unverified** | `status != 'verified'` | **None**: Verification has not yet completed, has failed, expired, or was revoked. |
| `digilocker_verified` | **DigiLocker Verified** | 1. `method = 'digilocker'` | **Highest**: Live government consent flow. Tamper-proof demographic data and photo attested directly by UIDAI via DigiLocker. |
| `document_secure_qr` | **Document Verified (Secure QR)** | 2. `method = 'qr'` OR `(method = 'ocr' AND qr_status = 'SECURE')` | **High**: Cryptographically validated offline 2048-bit digital signature (RSA PKCS#1 v1.5 SHA-256) from UIDAI Secure QR code. |
| `document_legacy_qr` | **Document Verified (Legacy QR)** | 3. `method = 'ocr'` AND `qr_status = 'PRIMITIVE'` | **Moderate**: Unencrypted old-format QR code cross-validated against OCR text fields. |
| `document_degraded_qr` | **Document Verified (OCR Degraded)** | 4. `method = 'ocr'` AND `qr_status = 'UNPROCESSABLE'` | **Degraded**: QR code was detected on the document but could not be decoded (worn, blurry, low resolution); verification completed on OCR text alone. |
| `document_ocr_only` | **Document Verified (OCR)** | 5. `method = 'ocr'` (Default / `qr_status = 'NOT_PRESENT'`) | **Standard**: Optical character recognition of visible card text combined with Cashfree document forgery/tamper checks. No cryptographic QR backing. |

#### Data Models & Shapes

##### TenantKYCView (Tenant Self-Service Projection)

```json
{
  "id": "3fa85f64-5717-4562-b3fc-2c963f66afa6",
  "status": "verified",
  "method": "digilocker",
  "masked_uid": "9812",
  "verified_at": "2026-09-23T10:00:00Z",
  "expires_at": "2027-09-23T10:00:00Z",
  "failure_reason": null,
  "created_at": "2026-09-23T09:58:00Z",
  "updated_at": "2026-09-23T10:00:00Z"
}
```

##### OwnerKYCVerificationView (Owner Property Review Projection)

```json
{
  "id": "3fa85f64-5717-4562-b3fc-2c963f66afa6",
  "status": "verified",
  "method": "ocr",
  "qr_status": "SECURE",
  "trust_tier": "document_secure_qr",
  "photo_stored": true,
  "name_mismatch": false,
  "masked_uid": "9812",
  "duplicate_detected": false,
  "is_dedupable": true,
  "verified_at": "2026-09-23T10:00:00Z",
  "expires_at": "2027-09-23T10:00:00Z",
  "failure_reason": null,
  "created_at": "2026-09-23T09:58:00Z",
  "updated_at": "2026-09-23T10:00:00Z"
}
```

##### OwnerKYCView (`GET /api/owner/tenants/:id/kyc`)

```json
{
  "tenant_id": "4da85f64-5717-4562-b3fc-2c963f66afa7",
  "verification": {
    "id": "3fa85f64-5717-4562-b3fc-2c963f66afa6",
    "status": "verified",
    "method": "digilocker",
    "qr_status": null,
    "trust_tier": "digilocker_verified",
    "photo_stored": true,
    "name_mismatch": false,
    "masked_uid": "9812",
    "duplicate_detected": false,
    "is_dedupable": true,
    "verified_at": "2026-09-23T10:00:00Z",
    "expires_at": "2027-09-23T10:00:00Z"
  },
  "audit_logs": [
    {
      "id": 1042,
      "tenant_id": "4da85f64-5717-4562-b3fc-2c963f66afa7",
      "actor": "tenant:4da85f64-5717-4562-b3fc-2c963f66afa7",
      "action": "consent_recorded",
      "detail_hash": "a1b2c3d4...",
      "prev_hash": "00000000...",
      "created_at": "2026-09-23T09:55:00Z"
    },
    {
      "id": 1043,
      "tenant_id": "4da85f64-5717-4562-b3fc-2c963f66afa7",
      "actor": "cashfree_webhook",
      "action": "verification_completed",
      "detail_hash": "e5f6g7h8...",
      "prev_hash": "a1b2c3d4...",
      "created_at": "2026-09-23T10:00:00Z"
    }
  ]
}
```

### Points, Community & Inspections

| Method | Path | Request / Query | Response / Notes |
| ------ | ---- | --------------- | ---------------- |
| GET | `/api/tenant/points` | None | `{ "balance": int, "expiring_soon": int, "earliest_expiry"?: string, "on_time_months": int, "freezes_left": int, "ledger": PointsLedgerEntry[] }`. |
| GET | `/api/tenant/rewards` | None | `{ "rewards": RewardView[] }` — catalog perks plus `eligible: boolean`, `reason?: string`. |
| POST | `/api/tenant/rewards/:id/redeem` | None | Redeems perk using available reward points. Returns `{ "redemption": RewardRedemption }`. |
| GET | `/api/tenant/inspections` | None | `{ "inspections": Inspection[] }` (includes inspection items). |
| POST | `/api/tenant/inspections/items/:id/dispute` | `{ "dispute_note": string }` (required) | Disputes an inspection item. Returns `{ "status": "disputed" }`. |
| GET | `/api/tenant/meal-rsvp` | None | Tomorrow UTC slots: `{ "date": string, "breakfast": boolean, "lunch": boolean, "dinner": boolean }`. |
| POST | `/api/tenant/meal-rsvp` | `{ "date": "YYYY-MM-DD", "slot": "breakfast" \| "lunch" \| "dinner", "attending": boolean }` | Submits meal attendance. Returns `{ "rsvp": MealRSVP }`. |
| GET | `/api/tenant/menu-poll` | None | Returns `{ "poll": MenuPoll, "votes": MenuVote[] }` or `{ "poll": null }`. |
| POST | `/api/tenant/menu-poll/vote` | `{ "poll_id": uuid, "option_id": uuid }` | Casts vote. Returns `{ "status": "voted" }`. |
| POST | `/api/tenant/hazards` | Multipart (`category`, `description`, file `photo?`) or JSON (`{ category, description, photo_base64? }`) | Submits hazard report. Returns `{ "hazard": Hazard }`. |
| GET | `/api/tenant/violations` | None | `{ "violations": Violation[] }` — own tenant violations only. |
| GET | `/api/tenant/leaderboard` | None | `{ "streaks": TenantStreak[], "floor_scores": FloorScore[] }`. |
| GET | `/api/tenant/referrals` | None | `{ "referrals": Referral[] }`. |
| POST | `/api/tenant/referrals` | `{ "phone": string, "name"?: string }` | Records prospective tenant referral. Returns `{ "referral": Referral }`. |

## Pay JSON

`GET /api/{owner|tenant}/dues/:id/pay` (VPA is **only** returned on this payload, never on `GET /api/owner/properties`):

```json
// Example 1: mode=manual (Direct UPI VPA QR)
{
  "mode": "manual",
  "vpa": "owner@upi",
  "upi_link": "upi://pay?pa=...",
  "note": "PG-XXXXXX",
  "due_code": "XXXXXX",
  "amount_paise": 1500000,
  "qr_png_url": "/api/tenant/dues/<id>/qr",
  "payable": true,
  "payment_session_id": null
}

// Example 2: mode=cashfree (Cashfree PG Checkout)
// Generated dynamically: calls Cashfree POST /orders via collector.Service.ensureCashfree
{
  "mode": "cashfree",
  "vpa": "",
  "upi_link": "",
  "note": "PG-XXXXXX",
  "due_code": "XXXXXX",
  "amount_paise": 1500000,
  "qr_png_url": "",
  "payable": true,
  "payment_session_id": "session_g7h8i9..."
}
```

- **Order Creation on Demand:** When `properties.payment_mode=cashfree` (or `payment_collection_mode=gateway`), calling `GET /api/tenant/dues/:id/pay`, `GET /api/owner/dues/:id/pay`, or resolving a public magic link automatically calls Cashfree's Order Create API (`POST /orders`) to generate a fresh `payment_session_id` and records a `payment_intents` row (`status: created`). The frontend passes this `payment_session_id` to Cashfree's Web/Mobile SDK (`startCashfree(payment_session_id)`).
- `payable` is `false` when the due is `paid` or `waived` — hide Pay / save / copy.
- `mode=cashfree` only when `properties.payment_mode=cashfree` **and** Cashfree credentials are configured.
- When `mode=cashfree`, PNG QR routes return **409** (personal VPA QR is replaced, not shown beside checkout). Use `/pay` JSON `payment_session_id` or the magic-link Cashfree button.
- CSV import, owner `match`, and `mark-cash-paid` remain active fallbacks after a Cashfree flip.

### Multi-Due Payment Options & Batch Checkout (§3.4)

#### 1. `GET /api/tenant/dues/options`

Aggregates open dues in strict FIFO chronological order (`due_date ASC, id ASC`) and returns distinct options based on the count of open dues:

```json
{
  "total_outstanding_paise": 1100000,
  "options": [
    {
      "option_type": "1",
      "label": "Oldest Due (1 Month)",
      "due_count": 1,
      "amount_paise": 550000,
      "due_ids": ["c1a40306-f6ab-476f-a894-984400ea0f01"],
      "dues": [
        {
          "id": "c1a40306-f6ab-476f-a894-984400ea0f01",
          "due_code": "RENT01",
          "kind": "rent",
          "amount_paise": 550000,
          "due_date": "2026-08-05T00:00:00Z"
        }
      ]
    },
    {
      "option_type": "all",
      "label": "Clear All Dues (2 Months)",
      "due_count": 2,
      "amount_paise": 1100000,
      "due_ids": [
        "c1a40306-f6ab-476f-a894-984400ea0f01",
        "d2b51417-e7bc-487f-b905-095511fb1a02"
      ],
      "dues": [
        {
          "id": "c1a40306-f6ab-476f-a894-984400ea0f01",
          "due_code": "RENT01",
          "kind": "rent",
          "amount_paise": 550000,
          "due_date": "2026-08-05T00:00:00Z"
        },
        {
          "id": "d2b51417-e7bc-487f-b905-095511fb1a02",
          "due_code": "RENT02",
          "kind": "rent",
          "amount_paise": 550000,
          "due_date": "2026-09-05T00:00:00Z"
        }
      ]
    }
  ]
}
```

*When 3 or more open dues exist, an additional option `"2"` (`"Oldest 2 Months"`) is populated.*

#### 2. `POST /api/tenant/dues/pay-batch`

Initiates multi-due checkout session. Accepts either `{ "option_type": "1"|"2"|"all" }` or `{ "due_ids": [uuid, ...] }`.
- **Reusable Intent Logic**: If an active reusable `payment_intent` with status `'created'` already covers the identical dues and has $\ge 10$ minutes TTL remaining, it is returned immediately without creating new Cashfree orders.
- **2-Phase Locking State Machine**:
  1. Under lock: Marks older intents for these dues as `'superseded'`. Inserts new intent and `payment_intent_dues` rows with status `'initiating'`, then commits.
  2. Zero locks held: Calls Cashfree `CreateUPIOrder`.
  3. Updates intent to `'created'` with `payment_session_id`.
- **Storage Double-Active Guard**: Backed by partial unique index `uq_active_intent_dues_item ON payment_intent_dues(due_id) WHERE status IN ('initiating', 'created')`. PostgreSQL trigger `trg_sync_payment_intent_dues_status` automatically cascades status transitions on `payment_intents` to `payment_intent_dues`.
- **Response**: Returns standard `domain.PayIntent` containing `payment_session_id` to initialize the Cashfree SDK.

## Webhooks

### 1. Cashfree Payment Gateway Webhook (`POST /webhooks/cashfree`)

- **Auth:** Unauthenticated public URL.
- **HMAC Verification:** `x-webhook-signature` = Base64(HMAC-SHA256(timestamp + rawBody, `CASHFREE_WEBHOOK_SECRET`)). `x-webhook-timestamp` must accompany signature.
- **Payload Handling:**
  - Settles `PAYMENT_SUCCESS_WEBHOOK` only when `payment_amount` equals `payment_intents.amount_paise`.
  - Settles via `settleMatched` with `matched_by=cashfree`.
  - Idempotent on `cf_payment_id` and `payments.upi_txn_id`.
  - `MarkPaid` executed only after settle or duplicate UTR resolution.
  - Unknown order → 200; DB errors → 500. Failed/dropped types return 200 and do not modify the due.

### 2. Cashfree Secure ID KYC Webhook (`POST /api/public/cashfree/kyc/webhook`)

- **Auth:** Unauthenticated public URL.
- **HMAC Verification:** Raw request body is read before JSON unmarshaling. Verified using `cashfree.VerifyWebhookHMAC` against `CASHFREE_WEBHOOK_SECRET`.
- **Payload Schema:**
  ```json
  {
    "type": "VERIFICATION_COMPLETED",
    "data": {
      "verification_id": "cf_sec_...",
      "status": "COMPLETED",
      "entity": "AADHAAR",
      "failed_reason": "optional error description"
    }
  }
  ```
- **Error & Retry Classification (ADR-004):**
  - **200 OK:** Returned for successful completions, idempotent re-deliveries of already terminal verifications, bad HMAC signatures (logged as security anomaly), malformed JSON payloads, and permanent 4xx upstream client errors. Drops delivery permanently without retry storms.
  - **500 Internal Server Error:** Returned for transient processing failures (DB disconnection, network timeout fetching verified document), prompting Cashfree to retry delivery with exponential backoff.

## Payment Reports & Proof Verification (ADR-005)

To support direct tenant-to-owner P2P transfers while gateway merchant onboarding is pending:

1. **Property Collection Mode:**
   - `properties.payment_collection_mode`: `'manual_proof'` (default) or `'gateway'`.
   - Forward-compatibility fields: `gateway_enabled_at`, `gateway_sub_merchant_id`.
2. **Payment Report Enhancements (Migration 015):**
   - `image_hash`: SHA-256 hex digest of uploaded payment receipt image.
   - `is_duplicate`: Flagged as `true` if identical image hash was previously submitted within the same property. **Flag, not hard-block.** Report is accepted into `pending_review` with duplicate badge for owner review.
   - OCR metadata fields: `ocr_amount`, `ocr_utr`, `ocr_txn_date`, `ocr_confidence`.
3. **Owner Confirmation as Sole Truth Anchor:**
   - Image hashing and OCR pre-fill confirmation inputs; they **never** mark a due paid automatically.
   - Owner confirmation (`POST /api/owner/payment-reports/:id/confirm`) executes `ManualMatch` and settles the due.

## Aadhaar KYC Architecture & Invariants (ADR-004)

1. **Assurance Tiers & Trust Badges:**
   - `digilocker_verified`: **Tier 1 (High Assurance)** — Live government-attested pull via Cashfree Secure ID direct API redirect.
   - `document_secure_qr`: **Tier 2 (High Cryptographic Assurance)** — Offline cryptographic verification of UIDAI Secure QR in-process (2048-bit RSA-SHA256 against `AADHAAR_QR_PUBLIC_KEY_PEM`) or Smart OCR extracted Secure QR payload.
   - `document_legacy_qr`: **Tier 3 (Medium Assurance)** — Primitive/legacy barcode without RSA envelope.
   - `document_degraded_qr`: **Tier 4 (Basic Document)** — Unprocessable barcode fallback.
   - `document_ocr_only`: **Tier 5 (Visual OCR)** — Clean text extraction without embedded QR signature.
   - `unverified`: Default state for unverified or pending tenants.

2. **Network-Before-Transaction & Concurrency Invariant:**
   - Outbound HTTP calls (Cashfree link creation, Smart OCR multipart upload, document fetch) are executed **outside** any database transaction.
   - Multi-instance double-submit and billing protection: `kyc_in_flight_lock` distributed lease table acquires a 30-second atomic lease (`TryAcquireInFlightLock`) before outbound network requests, with guaranteed deferred cleanup (`ReleaseInFlightLock(context.Background(), tenantID)`).

3. **Zero Demographic Persistence & No Profile Clobbering:**
   - Extracted `Name`, `DOB`, `Gender`, `Address` are ephemeral inputs to `domain.ComputeIdentityHash` and `domain.IsNameMatch`. They are **never stored** in Postgres or `kyc_audit_log` (DPDP Rule 8 compliance).
   - `CompleteVerificationTx` updates **only** `tenants.aadhaar_last4`. It never clobbers `tenants.name` or `tenants.id_photo_bytes` (which are collected during onboarding via `POST /join`).
   - Self-submitted join photo remains in `tenants.id_photo_bytes`. Government/vendor-attested Aadhaar crop is stored separately in `kyc_verification.attested_photo_bytes` (`photo_stored = TRUE`), accessible via `GET /api/owner/tenants/:id/kyc/photo`.

4. **Name Mismatch Evaluation & Zero-PII Audit:**
   - Evaluated across DigiLocker webhook, Smart OCR, and Secure QR. Flagged as boolean `name_mismatch` on `kyc_verification`.
   - In `kyc_audit_log`, recorded as zero-PII structured token (`action = 'name_mismatch'`, `detail = 'mismatch_detected=true'`).

5. **Symmetric Duplicate Detection:**
   - `CompleteVerificationTx` generates an irreversible HMAC-SHA256 identity hash:
     `HMAC-SHA256(secret, NormalizeName(name) | canonicalDOB | GENDER | maskedUID)`.
   - If a collision with an existing active tenant occurs, both the incoming verification and the colliding record are flagged: `duplicate_detected = TRUE`.
   - Verification is not hard-blocked; flagged for owner review.
   - False positives can be cleared by the owner via `POST /api/owner/tenants/:id/kyc/clear-duplicate` with an audited reason.

6. **Tamper-Evident Hash-Chained Audit Ledger:**
   - `kyc_audit_log` records all lifecycle actions (`consent_recorded`, `initiated`, `completed`, `failed`, `consent_revoked`, `duplicate_cleared`, `name_mismatch`).
   - Hash chain: `prev_hash` links to `id DESC`.
   - Immutability enforced by PostgreSQL trigger `trg_kyc_audit_immutable` raising an exception on any `UPDATE` or `DELETE`.

7. **DPDP Act 2023 Compliance & Revocation:**
   - On `POST /api/tenant/kyc/revoke`, an atomic transaction scrubs `masked_uid`, `identity_hash`, and `attested_photo_bytes` (`photo_stored = FALSE`) across `kyc_verification`, and clears `tenants.aadhaar_last4`.

8. **Projection Boundaries:**
   - Tenant view (`GET /api/tenant/kyc/status`, `GET /api/tenant/kyc/return`, `POST /api/tenant/kyc/upload`, `POST /api/tenant/kyc/qr`) strictly hides `identity_hash`, `duplicate_detected`, and `vendor_reference_id`.
   - Owner view (`GET /api/owner/tenants/:id/kyc`) includes `trust_tier`, `qr_status`, `photo_stored`, `name_mismatch`, `duplicate_detected`, `is_dedupable`, and full `audit_logs[]`, but excludes raw HMAC hashes.
   - Attested photo crop served via `GET /api/owner/tenants/:id/kyc/photo`.

## Global Search (RBAC)

Scoped by JWT role and `property_id` / `tenant_id` claims — never accept `property_id` from the client.

| Method | Path | Auth | Rate Limit | Query Parameters |
| ------ | ---- | ---- | ---------- | ---------------- |
| GET | `/api/search` | Any authenticated role (`owner`, `manager`, `tenant`) | 30 req/min (burst 10) per IP | `q` (required, min 2 chars), `limit` (1–50, default 20), `types` (comma-separated filter), `mode` (`lexical` default, `hybrid` if pgvector is enabled) |

**Role-Based Entity Scoping:**

| Role | Entity Types Returned |
| ---- | --------------------- |
| owner | `tenant`, `due`, `payment`, `payment_report`, `join_request`, `event`, `document` (hybrid) |
| manager | `tenant`, `inspection`, `hazard`, `violation`, `document` (hybrid) |
| tenant | `due`, `payment`, `document` (hybrid) — strictly scoped to caller's `claims.tenant_id` |

Deep-link `path` in results is dynamically rewritten per role (e.g. manager tenant results point to `/manager/inspections?q=...`; tenant dues point to `/tenant/dues?q=...`).

## Notifications

Scoped to `claims.UserID` at the repository layer. Owners, managers, and tenants share identical endpoints.

| Method | Path | Auth | Query / Payload | Response / Notes |
| ------ | ---- | ---- | --------------- | ---------------- |
| GET | `/api/notifications` | Any authenticated | Query: `cursor` (string), `limit` (1–50, default 20) | `{ "notifications": Notification[], "unread_count": int, "next_cursor"?: string }` |
| PATCH | `/api/notifications/:id/read` | Any authenticated | None | Marks single notification as read. Returns `{ "ok": true }`. |
| PATCH | `/api/notifications/read-all` | Any authenticated | None | Marks all notifications for user as read. Returns `{ "ok": true }`. |

**Notification Item Shape:**

```json
{
  "id": "c1f10825-9614-419b-b6d4-d53fb7c6bf9e",
  "source_event_id": 142,
  "recipient_id": "76ba3f53-271d-4076-96ad-d00730d8ebc1",
  "property_id": "31b6ea55-ec44-42b7-a3f1-f896b5fc24ee",
  "type": "recurring_tie_out_exception",
  "title": "⚠️ Recurring tie-out exception across 3 consecutive months",
  "deep_link": "/owner/finance/tie-out",
  "is_action_required": true,
  "read_at": null,
  "created_at": "2026-09-16T12:00:00Z"
}
```

- `is_action_required` (boolean): `true` if the notification represents an actionable pending item demanding immediate user intervention (e.g. pending expense approval, tie-out variance exception, payment report review, inspection dispute).

## Preferences & Locales

The localization system coordinates multi-lingual support across `pg-go` and `pg-react`. Supported locales are validated against a strict platform registry and stored per user in `user_preferences`.

### Supported Locales Registry

| Code | Name | Native Name | Default |
| :--- | :--- | :--- | :--- |
| `en-IN` | English | English | Yes (Fallback) |
| `te-IN` | Telugu | తెలుగు | No |
| `ta-IN` | Tamil | தமிழ் | No |
| `kn-IN` | Kannada | ಕನ್ನಡ | No |

### Endpoints

| Method | Path | Auth | Headers | Body / Query | Response / Notes |
| :--- | :--- | :--- | :--- | :--- | :--- |
| GET | `/api/locales` | Public | None | None | `{ "locales": LocaleMetadata[], "default_locale": "en-IN" }` |
| GET | `/api/me/preferences` | Any authenticated | `Authorization: Bearer <jwt>`, optional `Accept-Language` | None | `{ "locale": "en-IN", "has_saved_preference": boolean }` |
| PATCH | `/api/me/preferences` | Any authenticated | `Authorization: Bearer <jwt>` | `{ "locale": "te-IN" }` | `{ "locale": "te-IN", "has_saved_preference": true }` |

### Detailed Behavior & Error Contracts

#### 1. `GET /api/locales`
Returns the enabled platform locales catalog and fallback tag (`en-IN`). Used by client language pickers and onboarding flows.

#### 2. `GET /api/me/preferences`
Retrieves the active user's saved preference from `user_preferences`.
- If saved preference exists: returns `{ "locale": "<saved-code>", "has_saved_preference": true }`.
- If no saved preference exists (`pgx.ErrNoRows`) or preferences store is unconfigured: resolves locale from incoming `Accept-Language` header (defaulting to `en-IN`) and returns `{ "locale": "<resolved-code>", "has_saved_preference": false }`.

#### 3. `PATCH /api/me/preferences`
Updates or inserts (`ON CONFLICT (user_id) DO UPDATE`) the user's preferred language tag in `user_preferences`.

Validation & error responses:
| Status | Body | When |
| :--- | :--- | :--- |
| 400 | `{ "error": "locale is required", "code": "preferences.localeRequired" }` | Missing or empty `locale` field in JSON payload. |
| 400 | `{ "error": "unsupported locale", "code": "preferences.invalidLocale" }` | Provided tag is not in `localization.SupportedLocales` catalog. |
| 400 | `{ "error": "invalid locale", "code": "preferences.invalidLocale" }` | Postgres `23514` check constraint violation (`ErrInvalidLocale`). |
| 401 | `{ "error": "missing bearer token", "code": "auth.missingToken" }` / `{ "error": "invalid token", "code": "auth.invalidToken" }` | Missing or invalid JWT. |
| 500 | `{ "error": "internal error" }` | Preferences repository error. |

### Error Code Registry (Detailed Trigger Mappings)

<!-- ERROR_CODES_DETAIL_START -->
| Domain | Code | HTTP Status | Trigger Description |
| :--- | :--- | :--- | :--- |
| **Auth** | `auth.missingToken` | 401 | Authorization header missing or missing Bearer prefix |
| **Auth** | `auth.invalidToken` | 401 | JWT cryptographic verification failed or malformed |
| **Auth** | `auth.accessRevoked` | 403 | User sessions revoked or tenant marked vacated |
| **Auth** | `auth.profileIncomplete` | 403 | Tenant profile not yet completed via `/api/join` |
| **Auth** | `auth.forbidden` | 403 | JWT role does not have permission for the endpoint |
| **Auth** | `auth.alreadyActivated` | 403 | Pending join endpoint called with an already active tenant |
| **Auth** | `auth.invalidOtp` | 401 | Provided OTP code does not match stored hash |
| **Auth** | `auth.otpExpired` | 401 | Provided OTP code has exceeded TTL |
| **Auth** | `auth.otpLocked` | 401 | Too many failed OTP verification attempts |
| **Auth** | `auth.rateLimited` | 429 | Exceeded OTP request rate limit (3/min) |
| **Auth** | `auth.noAccount` | 404 | Phone or email not found and no valid invite code provided |
| **Auth** | `auth.invalidInvite` | 404 | Provided invite code does not exist or has been rotated |
| **Auth** | `auth.firebaseNotConfigured` | 503 | Firebase Admin SDK credentials not configured on backend |
| **Auth** | `auth.emailNotVerified` | 403 | Google account email address is not verified |
| **Auth** | `auth.invalidFirebaseToken` | 401 | Firebase ID token verification failed |
| **Auth** | `auth.unauthorized` | 401 | Request claims do not contain a valid user ID |
| **Auth** | `auth.noPropertyScope` | 403 | Claims do not contain a valid property scope |
| **Join** | `join.invalidInvite` | 404 | Invite code not found for join request |
| **Join** | `join.noPendingRequest` | 404 | No pending join request found for tenant |
| **Join** | `join.notFound` | 404 | Join request record not found |
| **Join** | `join.alreadyOnboarded` | 409 | Join request has already been completed |
| **Join** | `join.alreadyActive` | 409 | Tenant is already active |
| **Join** | `join.nameRequired` | 400 | Full name is required for profile submission |
| **Join** | `join.consentRequired` | 400 | Legal and data consent must be explicitly accepted |
| **Join** | `join.photoRequired` | 400 | ID photo upload is required for verification |
| **Join** | `join.profileIncomplete` | 400 | Join profile missing mandatory fields before activation |
| **Join** | `join.requestNotPending` | 400 | Join request is not in pending state |
| **Join** | `join.notAwaitingAssignment` | 400 | Join request is not awaiting room/rent assignment |
| **Payment** | `payment.duplicateTxn` | 409 | UPI transaction ID or UTR already recorded |
| **Payment** | `payment.cashPartialNotAllowed` | 400 | Cash payments must match the exact remaining due balance |
| **Payment** | `payment.dueNotOpen` | 400 | Due is already paid, waived, or not payable |
| **Payment** | `payment.noDepositDue` | 400 | No pending security deposit due found for tenant |
| **Payment** | `payment.emptyTxnId` | 400 | UTR or transaction ID was empty or blank |
| **Payment** | `payment.ambiguousMatch` | 400 | Multiple dues match amount within date window |
| **Payment** | `payment.noMatch` | 400 | No open due matches the provided payment amount |
| **Payment** | `payment.invalidAmount` | 400 | Payment amount in paise must be positive integer |
| **Finance** | `finance.duplicateRequest` | 409 | Mutating financial request with duplicate Idempotency-Key |
| **Finance** | `finance.idempotencyRequired` | 400 | Idempotency-Key header is missing on mutating financial endpoint |
| **Finance** | `finance.invalidAmount` | 400 | Financial amount in paise must be positive integer |
| **Finance** | `finance.invalidKind` | 400 | Invalid capital transaction or payment kind |
| **Finance** | `finance.overpay` | 400 | Expense payment amount exceeds remaining balance |
| **Finance** | `finance.expenseNotPayable` | 400 | Expense is pending maker-checker approval or already paid |
| **Finance** | `finance.policyExceeded` | 400 | Expense exceeds manager discretionary spending policy limit |
| **Finance** | `finance.approvalRequired` | 400 | Expense requires owner approval before disbursement |
| **Finance** | `finance.periodNotCloseable` | 400 | Period tie-out has unexplained variance, or the period has not yet ended, and cannot close |
| **Finance** | `finance.periodClosed` | 409 | Posting or recompute targets a closed accounting period (ledger controls C-3/C-4) |
| **Finance** | `finance.periodNotReopenable` | 400 | Period is not closed or cannot be reopened |
| **Finance** | `finance.notFound` | 404 | Financial transaction, budget, or expense record not found |
| **Finance** | `finance.forbidden` | 403 | User does not have authorization for this financial mutation |
| **Finance** | `finance.disabled` | 503 | Financial intelligence engine is disabled in property settings |
| **Finance** | `finance.expenseNotVoidable` | 409 | Expense is already cancelled or paid; only pending or approved expenses can be voided |
| **Finance** | `finance.expenseStateChanged` | 409 | Expense status changed between read and write; the client can retry |
| **Finance** | `finance.reasonRequired` | 400 | Void reason must be 3 to 500 characters |
| **Finance** | `finance.dateOutOfRange` | 400 | Expense `occurred_at` is in the future or older than 90 days |
| **Request** | `request.invalidBody` | 400 | Request JSON payload malformed or missing required fields |
| **Request** | `request.invalidId` | 400 | UUID URL parameter is malformed |
| **Request** | `request.dueDayInvalid` | 400 | Due day must be an integer between 1 and 28 |
| **Request** | `request.imageTooLarge` | 400 | Uploaded image exceeds maximum allowed size (2MB) |
| **Request** | `request.imageReadFailed` | 400 | Server failed to read uploaded multipart file stream |
| **Preferences** | `preferences.localeRequired` | 400 | Missing locale field in preferences payload |
| **Preferences** | `preferences.invalidLocale` | 400 | Locale tag not supported or failed database check constraint |
| **KYC** | `kyc.no_consent` | 409 | No active DPDP consent recorded for tenant prior to initiating verification |
| **KYC** | `kyc.already_verified` | 409 | Tenant already has an active, valid KYC verification on file |
| **KYC** | `kyc.in_flight` | 409 | A verification attempt is already in-progress for this tenant (prevents double-billing) |
| **KYC** | `kyc.consent_revoked` | 409 | Tenant consent was previously revoked under DPDP Rule 8 |
| **KYC** | `kyc.qr_invalid` | 422 | Aadhaar Secure QR RSA-SHA256 signature verification failed against UIDAI root cert |
| **KYC** | `kyc.qr_incomplete` | 422 | Aadhaar demographic fields missing (Name, UID last 4, DOB/YOB) in QR or Smart OCR |
| **KYC** | `kyc.digilocker_unavailable` | 503 | Cashfree Secure ID service or credentials not configured / upstream service unavailable |
| **KYC** | `kyc.not_found` | 404 | KYC verification record not found for tenant |
<!-- ERROR_CODES_DETAIL_END -->

## Finance, ROI, Payouts & Departures (Rev 13)

All financial amounts are in **bigint paise** (integer). Zero floating-point arithmetic.

### Idempotency Discipline
All mutating financial endpoints require an `Idempotency-Key` header.
- Header: `Idempotency-Key: <unique-client-key>`
- If missing: `400 Bad Request` (`finance.idempotencyRequired`)
- If duplicate within property: `409 Conflict` (`finance.duplicateRequest`)

### Owner Finance Routes

| Method | Path | Body / Query | Response / Notes |
| :--- | :--- | :--- | :--- |
| GET | `/api/owner/finance/summary` | Query: `period=YYYY-MM` | `{ period, collections: ReconciliationSummary, operating: OperatingSummary, query_keys, invalidate_with }`. Explicit labels: `"Collections (reconciliation)"` vs `"Operating view (ledger)"`. |
| GET | `/api/owner/finance/ledger` | Query: `period`, `from`, `to`, `account` | `{ entries: JournalLine[] }`. Balanced double-entry lines. |
| POST | `/api/owner/finance/capital` | `{ kind: "initial"\|"additional"\|"withdrawal", amount_paise, purpose }` | **201** `{ capital: CapitalTransaction }`. Header: `Idempotency-Key`. Equity transaction. |
| GET | `/api/owner/finance/capital` | None | `{ capital: CapitalTransaction[] }`. |
| POST | `/api/owner/finance/expenses` | `{ category_code, vendor_name, description, amount_paise, emergency?, room_id? }` | **201** `{ expense: Expense, approval?: ApprovalRequest }`. Header: `Idempotency-Key`. |
| GET | `/api/owner/finance/expenses` | None | `{ expenses: Expense[] }`. |
| POST | `/api/owner/finance/expenses/:id/payments` | `{ amount_paise, method: "cash"\|"upi"\|"bank" }` | **201** `{ payment: ExpensePayment }`. Header: `Idempotency-Key`. Partial payments allowed. |
| GET | `/api/owner/finance/advances` | None | `{ advances: ManagerAdvance[], outstanding_paise: int64 }`. Out-of-pocket manager advances. |
| POST | `/api/owner/finance/reimbursements` | `{ manager_user_id, amount_paise }` | **201** `{ reimbursement: ManagerReimbursement }`. Header: `Idempotency-Key`. Reimburses manager advances. |
| GET | `/api/owner/finance/budgets` | Query: `period=YYYY-MM` | `{ budgets: Budget[] }`. |
| POST | `/api/owner/finance/budgets` | `{ category_code, period_month, amount_paise }` | **201** `{ budget: Budget }`. Upsert monthly budget. |
| PATCH | `/api/owner/finance/budgets/:id` | Same as POST | **201** `{ budget: Budget }`. Delegates to `PostBudget`; same response shape. |
| GET | `/api/owner/finance/settings` | None | `{ settings: PropertyFinanceSettings, policy: ApprovalPolicy, loyalty: PropertyGamificationSettings }`. Merged financial knobs. |
| PATCH | `/api/owner/finance/settings` | `{ settings?, policy?, loyalty? }` | `{ settings, policy, loyalty? }`. **Atomic single-DB transaction** update across all tables. |
| GET | `/api/owner/finance/policies` | None | Alias for `/api/owner/finance/settings`. |
| PATCH | `/api/owner/finance/policies` | Same as `/finance/settings` | Alias for `/api/owner/finance/settings`. |
| GET | `/api/owner/finance/approvals` | Query: `status=pending` | `{ approvals: ApprovalRequest[] }`. Maker-checker expense review queue. |
| POST | `/api/owner/finance/approvals/:id/:action` | `{ note? }` (`action` is `approve` or `reject`) | `{ ok: true }`. Approves or rejects expense/advance/budget. |
| GET | `/api/owner/finance/tie-out` | Query: `period=YYYY-MM` | `{ tie_out: PeriodTieOut, label: "Collections tie-out" }`. Reconciles collections control to ledger collection lines. |
| POST | `/api/owner/finance/tie-out/close` | Query: `period=YYYY-MM` | `{ tie_out: PeriodTieOut }`. Blocks with `400/422` if unexplained difference `!= 0`. |
| GET | `/api/owner/finance/variance-bridge` | Query: `period=YYYY-MM` | `{ variance_bridge: VarianceBridge, label: "Plan vs actual" }`. Decomposes budget vs actual OCF. |
| GET | `/api/owner/finance/imports` | None | `{ suggestions: ExpenseImportSuggestion[] }`. Unmatched debit CSV suggestions. |

### Owner ROI & Intelligence Routes

| Method | Path | Body / Query | Response / Notes |
| :--- | :--- | :--- | :--- |
| GET | `/api/owner/roi` | Query: `period=YYYY-MM` | `{ roi: { period, occupancy, operating, break_even, recovery, official } }`. `official: true` only after tie-out closed. |
| GET | `/api/owner/roi/recovery` | Query: `period=YYYY-MM` | Same as `/api/owner/roi`. |
| GET | `/api/owner/roi/break-even` | Query: `period=YYYY-MM` | Same as `/api/owner/roi`. |
| POST | `/api/owner/roi/scenario` | `{ current_occupancy_bps, target_occupancy_bps, capacity_beds, rent_per_bed_paise, fixed_opex_paise, variable_opex_per_bed_paise }` | `{ scenario: ScenarioResult }`. What-if break-even simulator. |
| GET | `/api/owner/insights` | None | `{ leakage: LeakageEvent[], recommendations: Recommendation[], coi_6m_paise, query_keys }`. |
| GET | `/api/owner/leakage` | None | Same as `/insights`. |
| GET | `/api/owner/leakage/:id` | None | `{ leakage: LeakageEvent }`. |
| GET | `/api/owner/recommendations` | None | `{ recommendations: Recommendation[] }`. |
| POST | `/api/owner/recommendations/:id/:action` | `{ realized_savings_paise? }` (`action` is `accept`\|`reject`\|`complete`) | `{ recommendation: Recommendation }`. |
| GET | `/api/owner/coi` | None | Same as `/insights`. Cost of Inaction. |
| GET | `/api/owner/forecast` | None | `{ forecast_ocf_paise: int64[], horizon_months: 12 }`. OCF forward forecast. |

### Manager Finance Routes

| Method | Path | Body / Query | Response / Notes |
| :--- | :--- | :--- | :--- |
| GET | `/api/manager/finance/expenses` | None | `{ expenses: Expense[] }`. Scoped to manager property. |
| POST | `/api/manager/finance/expenses` | `{ category_code, vendor_name, description, amount_paise, emergency?, room_id? }` | **201** `{ expense: Expense, approval?: ApprovalRequest }`. Checked against `approval_policies` in DB. Header: `Idempotency-Key`. |
| POST | `/api/manager/finance/expenses/:id/payments` | `{ amount_paise, method }` | **201** `{ payment: ExpensePayment }`. Out-of-pocket manager payment recorded as `manager_advances`. Header: `Idempotency-Key`. |
| GET | `/api/manager/finance/today` | None | `{ expenses: Expense[], pending_approvals: ApprovalRequest[], kitchen_headcount: KitchenHeadcount, hide_capital: boolean }`. Operational finance dashboard. `hide_capital` is `true` when caller is a manager **and** `ManagerCanViewCapital` is `false` in property finance settings. |
| POST | `/api/manager/finance/meal-prep` | `{ meal_date, meal_slot: "breakfast"\|"lunch"\|"dinner", prepared_count, discarded_count? }` | `{ prep: MealPrepActual }`. Actuals for food leakage detector. |

### pg-react Query Key Succession & Invalidation Rules

When financial mutations succeed in pg-react:
1. **Collections & Dues**: Invalidate `['owner', 'reconciliation']`, `['owner', 'finance', 'summary']`, `['owner', 'finance', 'tie-out']`, `['owner', 'roi']`.
2. **Expenses & Payments**: Invalidate `['owner', 'finance', 'summary']`, `['owner', 'finance', 'expenses']`, `['owner', 'finance', 'advances']`, `['owner', 'finance', 'variance-bridge']`, `['owner', 'roi']`.
3. **Reimbursements**: Invalidate `['owner', 'finance', 'summary']`, `['owner', 'finance', 'advances']`, `['owner', 'finance', 'ledger']`.
4. **Tie-Out Close**: Invalidate `['owner', 'finance', 'tie-out']`, `['owner', 'roi']`.
5. **Departures & Payouts**: Invalidate `['owner', 'tenants']`, `['owner', 'dues']`, `['owner', 'payouts', 'items']`, `['owner', 'payouts', 'batches']`, `['owner', 'payouts', 'payees']`, `['owner', 'finance', 'summary']`, `['owner', 'finance', 'ledger']`.

---

### Universal Lock Hierarchy & Concurrency Specification (ADR-008)

To eliminate deadlock potential and race conditions across multi-table mutating transactions, `pg-go` strictly enforces a 15-entity topological lock hierarchy:

$$\text{tenants (1)} \prec \text{tenant\_streaks (2)} \prec \text{dues (3)} \prec \text{payment\_intents (4)} \prec \text{payment\_intent\_dues (5)} \prec \text{payments (6)} \prec \text{payment\_allocations (7)} \prec \text{gateway\_refunds (8)} \prec \text{refund\_allocations (9)} \prec \text{tenant\_departures (10)} \prec \text{departure\_deductions (11)} \prec \text{departure\_due\_adjustments (12)} \prec \text{payout\_payees (13)} \prec \text{payout\_batches (14)} \prec \text{payout\_items (15)}$$

**Core Invariants**:
1. **Strict Partial Ordering**: Transactions acquire row-level exclusive locks (`FOR UPDATE`) in strictly non-decreasing level order. If a transaction does not read-lock or mutate an entity level, it skips that level entirely without violating the sequence.
2. **Secondary Sorter Discipline**: Within any single level that touches multiple rows (such as multiple `dues` or multiple `payments`), rows are locked deterministically by secondary attributes:
   - `dues`: `ORDER BY due_date ASC, id ASC`
   - `payment_allocations`: `ORDER BY due_id ASC, id ASC`
   - `gateway_refunds`: `ORDER BY created_at ASC, id ASC`
   - `departure_deductions`: `ORDER BY created_at ASC, id ASC`
3. **Zero Network I/O Under Lock**: Network calls to external payment aggregators (Cashfree Order Create, Cashfree Refund Create, DigiLocker redirect/fetch) are strictly prohibited while holding database row locks. All order/refund rows are committed in `'initiating'` or `'initiated'` state before outbound I/O.

---

### Canonical SSoT Due Status Recomputation & Invariants

Due balance and settlement status across the entire application are derived strictly through a pure, centralized Single Source of Truth function:

$$\text{RecomputeDueStatusMath}(\text{netPaid}, \text{contractualCeiling}) \longrightarrow (\text{status}, \text{amount})$$

#### Net Paid Invariant
$$\text{Net Paid on Due} = \sum \text{payment\_allocations} - \sum \text{refund\_allocations}_{\text{(succeeded)}} - \sum \text{departure\_due\_adjustments}$$

Implemented in [`PaymentRepo.GetDueNetPaidPaise`](file:///c:/Users/divak/Downloads/pg-go/internal/postgres/payment_repo.go#L403-L411).

#### Contractual Ceiling Persistence
- On standard billing, the ceiling is `due.original_amount`.
- When a due is prorated on departure, `due.contractual_ceiling_paise` persists `proratedRentPaise` permanently in the database.
- Even after full settlement (`due.amount = 0`), `contractual_ceiling_paise` survives. If an owner refund subsequently hits the payment, `recomputeDueStatusUnderLock` computes remaining balance against `contractual_ceiling_paise` rather than `original_amount`.

#### Guard A (Settlement Terminal Boundary Guard)
`OwnerRefundPayment` (`POST /api/owner/payments/:id/refund`) enforces an atomic boundary under lock:
- If `tenant_departures.status IN ('approved', 'refunded')` for the tenant, or
- If any linked due has `departure_due_adjustments` rows,
- The API immediately rejects the refund with **HTTP 400 Bad Request**:
  > *"Cannot issue gateway refund against payment tied to a settled departure. Deposit and rent adjustments must be resolved via departure disbursement/payout ledger."*

#### Guard B (SSoT Status Math)
In [`domain.RecomputeDueStatusMath`](file:///c:/Users/divak/Downloads/pg-go/internal/domain/due.go#L260):
- If $\text{netPaid} \ge \text{ceiling} \implies \text{status} = \text{'paid'}, \text{amount} = 0$
- If $0 < \text{netPaid} < \text{ceiling} \implies \text{status} = \text{'partial'}, \text{amount} = \text{ceiling} - \text{netPaid}$
- If $\text{netPaid} \le 0 \implies \text{status} = \text{'pending'}, \text{amount} = \text{ceiling}$

---

### Tenant Departure Settlement Engine (ADR-009)

Executed via `POST /api/owner/departures/:id/settle` (`SettleDepartureUnderLock`). The settlement engine evaluates four distinct operational scenarios:

1. **Scenario 1 (Unpaid Rent at Departure)**:
   - Tenant paid ₹0 towards monthly rent; stayed 15 days ($\text{prorated} = ₹2,750$).
   - Shortfall is netted from deposit via internal payment (`matched_by = 'deposit_netting'`, `provider = 'internal'`).
   - Rent due transitions to `status = 'paid'`, `amount = 0`, `contractual_ceiling_paise = 275000`.
   - `prorated_rent_owed_paise = 275000`, `unused_rent_refund_paise = 0`.
2. **Scenario 2 (Advance Rent Paid in Full)**:
   - Tenant prepaid full ₹5,500 on Day 1; stayed 15 days ($\text{prorated} = ₹2,750$).
   - Excess ₹2,750 is refunded to the tenant as an unused rent credit.
   - Inserted into `departure_due_adjustments` with `adjustment_type = 'unused_rent_reversal'`.
   - Rent due remains `status = 'paid'`, `contractual_ceiling_paise = 275000`.
   - `unused_rent_refund_paise = 275000`, `prorated_rent_owed_paise = 0`.
3. **Scenario 3 (Partial Payment Above Prorated Owed)**:
   - Tenant paid ₹3,000 on Day 1; stayed 15 days ($\text{prorated} = ₹2,750$).
   - Excess ₹250 inserted into `departure_due_adjustments`.
   - Rent due transitions to `status = 'paid'`, `amount = 0`, `contractual_ceiling_paise = 275000`.
   - `unused_rent_refund_paise = 25000`, `prorated_rent_owed_paise = 0`.
4. **Scenario 4 (Negative Net Refund / Receivable Balance)**:
   - Damages and unpaid rent exceed total available credits ($\text{deposit} + \text{unused rent refund}$).
   - `net_refund_paise = 0` (strictly satisfying database check constraint `net_refund_paise >= 0`).
   - `receivable_balance_paise = totalDebits - totalCredits`.
   - No `payout_items` row is generated (`payoutItem == nil`), and no payee is required.
   - Tenant is marked `vacated`. Double-entry accounting mirror balances with Dr `tenant_receivable`.

#### Pre-Settle Only Departure Cancellation
- Departure cancellation (`status = 'cancelled'`) is strictly permissible only prior to settlement (`pending` or `inspected`).
- Once `SettleDepartureUnderLock` commits, the departure is financially and contractually terminal. Rollbacks are prohibited; any returning tenant is processed as a fresh onboarding.

---

### Operational Payouts Subsystem & Batches (ADR-009)

1. **Payee Registry & PII Protection**:
   - `POST /api/owner/payouts/payees` stores payout destinations for staff, vendors, tenant deposits, and guardian deposits.
   - Plaintext account numbers are never exposed in search or API lists (`account_number_last4` only).
   - Enforces HMAC-SHA256 account hashing (`account_number_hash`) using property secrets to prevent cross-property duplicate payee creation without decrypting database backups.
2. **Atomic Batch Packaging**:
   - `POST /api/owner/payouts/batches` packages all pending unbatched payout items (`status = 'pending'`, `batch_id = NULL`) into an immutable `payout_batches` row with status `'draft'`.
   - Generates deterministic batch numbers: `BATCH-YYYYMMDD-<hex>`.
   - Computes HMAC-SHA256 checksum over concatenated item fields (`reference_number|payee_id|amount_paise|purpose|period_label`), stored in `payout_batches.file_checksum`.
3. **Bank Instruction Sheet Export**:
   - `GET /api/owner/payouts/batches/:id/export` streams the CSV instruction sheet with response header `X-Batch-Checksum: <hmac-sha256>`.
   - Prevents file tampering or formula injection during bank upload.

---

### Gamification Minor Protection & Streak Soft-Landing (§3.6 & §3.7)

1. **DPDP Act 2023 Minor Protection**:
   - Tenant DOB is evaluated against current time. If age < 18 years, `is_minor` evaluates to `true`.
   - For minor tenants, `GamificationActive()` strictly evaluates to `false`.
   - Minor tenants cannot earn reward points, cannot activate gamification streaks, and are suppressed from public leaderboard rankings.
   - Minors require verified guardian KYC (`guardian_name`, `guardian_phone`, `guardian_relation`, `guardian_kyc_reference_id`, `guardian_consent_verified_at`).
2. **Streak Soft-Landing & Grace Window**:
   - Tenants receive a 2-calendar-day grace window after due date before a payment is classified as late.
   - On a late payment where no streak freezes remain, streaks do not reset to 0: they experience a **soft-landing decrement** (e.g. Month 8 $\rightarrow$ Month 7).
   - Milestone awards are strictly deduplicated by unique constraint `uq_tenant_milestone_award(tenant_id, milestone_code)`.
   - Single-shot replay protection on due events via `tenant_streak_due_events(due_id)`.

---

## Public

| Method | Path | Auth | Notes |
| ------ | ---- | ---- | ----- |
| GET | `/healthz` | Public | `{ "status": "ok" }` |
| GET | `/api/healthz` | Public | `{ "status": "ok" }` |
| GET | `/api/locales` | Public | Supported locale registry: `{ "locales": LocaleMetadata[], "default_locale": "en-IN" }` |
| GET | `/api/push/vapid-public-key` | Public | `{ "public_key": "..." }` |
| POST | `/webhooks/cashfree` | Public | Cashfree Payment Gateway settlement webhook (HMAC verified). |
| POST | `/api/public/cashfree/kyc/webhook` | Public | Cashfree Secure ID verification completion webhook (raw HMAC verified). |
| GET | `/p/:token` | Public | Standalone HTML payment page (save QR, copy VPA/note, open UPI). `/p/[REDACTED]` in server logs. |
| POST | `/p/:token/push/subscribe` | Public | Web push subscription directly from payment link page. |

### Cashfree Secure ID Webhook Specification

The KYC subsystem accepts asynchronous verification outcomes from Cashfree Secure ID at `POST /api/public/cashfree/kyc/webhook`.

#### Security & Signature Verification
- **Headers Required**:
  - `x-webhook-timestamp`: Epoch timestamp (seconds or milliseconds) or ISO string provided by Cashfree.
  - `x-webhook-signature`: HMAC-SHA256 signature in Base64 encoding.
- **Verification Rule**: The raw body bytes must be read directly from the request stream **before** any JSON parsing. The signature is computed as `HMAC-SHA256(CASHFREE_WEBHOOK_SECRET, timestamp + rawBodyBytes)`.
- If HMAC verification fails, the server logs a security warning and returns `200 OK` (to prevent Cashfree retry storms on invalid callers).

#### Webhook Payload Schema
```json
{
  "type": "VERIFICATION_COMPLETED",
  "data": {
    "verification_id": "3fa85f64-5717-4562-b3fc-2c963f66afa6",
    "status": "COMPLETED",
    "entity": "AADHAAR",
    "failed_reason": ""
  }
}
```
*Note: `type` may also be `VERIFICATION_FAILED`, in which case `failed_reason` contains the failure explanation.*

#### Processing & Delivery Semantics
- **Network-Before-Transaction**: On `VERIFICATION_COMPLETED`, the handler invokes Cashfree Secure ID `GET /verification/digilocker/:id/document` over HTTP outside any DB transaction to fetch demographic fields (`name`, `dob`, `gender`, `masked_uid`) and base64 photo.
- **Idempotency**: If the verification record has already transitioned to a terminal status (`verified` or `failed`), the handler immediately returns `200 OK` without side effects.
- **HTTP Response Codes**:
  - `200 OK`: Successful processing, idempotent no-op, unrecognized vendor reference ID, or bad HMAC signature.
  - `500 Internal Server Error`: Transient failure (database lock/timeout, temporary upstream 5xx/network error fetching document) allowing Cashfree exponential retries.


## Domain Data Models (Rev 13)

### 1. Tenant (Updated with Minor Protection & Guardian KYC)
```json
{
  "id": "c1a40306-f6ab-476f-a894-984400ea0f01",
  "property_id": "31b6ea55-ec44-42b7-a3f1-f896b5fc24ee",
  "name": "Arjun Sharma",
  "phone": "+919876543210",
  "room_number": "101",
  "rent_amount": 550000,
  "due_day": 5,
  "notice_period_days": 30,
  "credit_balance_paise": 0,
  "status": "active",
  "permanent_address": "Flat 4B, Residency, Hyderabad",
  "current_address": "Room 101, PG Hostel, Bangalore",
  "parent_name": "Ramesh Sharma",
  "emergency_phone": "+919876543219",
  "joined_on": "2026-08-01T00:00:00Z",
  "has_id_photo": true,
  "aadhaar_last4": "1234",
  "majority_date": "2027-05-20T00:00:00Z",
  "guardian_name": "Ramesh Sharma",
  "guardian_phone": "+919876543219",
  "guardian_relation": "father",
  "guardian_kyc_reference_id": "kyc_guard_4a71...",
  "guardian_consent_verified_at": "2026-08-01T10:00:00Z",
  "is_gamification_disabled": false,
  "is_minor": false,
  "created_at": "2026-08-01T09:00:00Z",
  "updated_at": "2026-09-01T10:00:00Z"
}
```

### 2. Due (Updated with Contractual Ceiling Persistence)
```json
{
  "id": "d2b51417-e7bc-487f-b905-095511fb1a02",
  "due_code": "RENT02",
  "tenant_id": "c1a40306-f6ab-476f-a894-984400ea0f01",
  "property_id": "31b6ea55-ec44-42b7-a3f1-f896b5fc24ee",
  "kind": "rent",
  "amount": 0,
  "original_amount": 550000,
  "contractual_ceiling_paise": 275000,
  "period_start": "2026-09-01T00:00:00Z",
  "period_end": "2026-09-30T23:59:59Z",
  "due_date": "2026-09-05T00:00:00Z",
  "status": "paid",
  "paid_at": "2026-09-15T14:30:00Z",
  "created_at": "2026-09-01T00:00:00Z",
  "updated_at": "2026-09-15T14:30:00Z"
}
```

### 3. Gateway Refund & Allocation
```json
{
  "id": "e3c62528-f8cd-4980-ba16-106622fc2b03",
  "payment_id": "f4d73639-09de-4a91-cb27-2177330d3c04",
  "property_id": "31b6ea55-ec44-42b7-a3f1-f896b5fc24ee",
  "provider": "cashfree",
  "cf_refund_id": "cf_ref_987654321",
  "refund_reference": "rf_1e8ec57be2cf4770bf158752352c4a4b",
  "idempotency_key": "idem_12345",
  "amount_paise": 100000,
  "status": "succeeded",
  "source": "owner",
  "reason": "Adjustment requested by tenant",
  "created_at": "2026-09-16T10:00:00Z",
  "updated_at": "2026-09-16T10:01:00Z"
}
```

### 4. Tenant Departure
```json
{
  "id": "76ba3f53-271d-4076-96ad-d00730d8ebc1",
  "tenant_id": "c1a40306-f6ab-476f-a894-984400ea0f01",
  "property_id": "31b6ea55-ec44-42b7-a3f1-f896b5fc24ee",
  "notice_given_at": "2026-08-15T10:00:00Z",
  "planned_vacate_date": "2026-09-15T00:00:00Z",
  "actual_vacate_date": "2026-09-15T00:00:00Z",
  "inspected_at": "2026-09-15T09:00:00Z",
  "sla_deadline_at": "2026-09-16T09:00:00Z",
  "deposit_amount_paise": 1000000,
  "unused_rent_refund_paise": 275000,
  "prorated_rent_owed_paise": 0,
  "outstanding_dues_netted_paise": 0,
  "deductions_paise": 100000,
  "net_refund_paise": 1175000,
  "receivable_balance_paise": 0,
  "status": "approved",
  "notes": "Keys handed over, room in good shape",
  "created_at": "2026-08-15T10:00:00Z",
  "updated_at": "2026-09-15T10:00:00Z"
}
```

### 5. Departure Deduction & Due Adjustment
```json
// Departure Deduction
{
  "id": "87cb4064-382e-4187-a7be-e11841e9fcd2",
  "departure_id": "76ba3f53-271d-4076-96ad-d00730d8ebc1",
  "description": "Deep cleaning and wall touchup",
  "amount_paise": 100000,
  "evidence_photo_key": "departures/76ba3f53/cleaning.jpg",
  "status": "agreed",
  "tenant_acknowledged_at": "2026-09-15T09:30:00Z",
  "created_at": "2026-09-15T09:15:00Z"
}

// Departure Due Adjustment
{
  "id": "98dc5175-493f-4298-b8cf-f22952fa0de3",
  "departure_id": "76ba3f53-271d-4076-96ad-d00730d8ebc1",
  "due_id": "d2b51417-e7bc-487f-b905-095511fb1a02",
  "amount_paise": 275000,
  "adjustment_type": "unused_rent_reversal",
  "created_at": "2026-09-15T10:00:00Z"
}
```

### 6. Payout Payee, Batch & Item
```json
// Payout Payee
{
  "id": "a9ed6286-5040-43a9-c9d0-0330630b1ef4",
  "property_id": "31b6ea55-ec44-42b7-a3f1-f896b5fc24ee",
  "payee_type": "tenant_deposit",
  "name": "Arjun Sharma",
  "phone": "+919876543210",
  "account_number_last4": "5678",
  "account_number_hash": "a1b2c3d4e5f6...",
  "ifsc": "HDFC0000123",
  "bank_name": "HDFC Bank",
  "key_version": 1,
  "is_verified": true,
  "created_at": "2026-09-15T09:45:00Z",
  "updated_at": "2026-09-15T09:45:00Z"
}

// Payout Batch
{
  "id": "bafe7397-6151-44ba-dae1-1441741c2f05",
  "property_id": "31b6ea55-ec44-42b7-a3f1-f896b5fc24ee",
  "batch_number": "BATCH-20260915-1a2b3c",
  "format_type": "instruction_sheet",
  "status": "draft",
  "total_amount_paise": 1175000,
  "item_count": 1,
  "created_by": "76ba3f53-271d-4076-96ad-d00730d8ebc1",
  "file_checksum": "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
  "created_at": "2026-09-15T10:30:00Z",
  "updated_at": "2026-09-15T10:30:00Z"
}

// Payout Item
{
  "id": "cb0f84a8-7262-45cb-ebf2-2552852d3016",
  "batch_id": "bafe7397-6151-44ba-dae1-1441741c2f05",
  "payee_id": "a9ed6286-5040-43a9-c9d0-0330630b1ef4",
  "departure_id": "76ba3f53-271d-4076-96ad-d00730d8ebc1",
  "reference_number": "dep_76ba3f53271d407696add00730d8ebc1",
  "amount_paise": 1175000,
  "purpose": "tenant_deposit_refund",
  "period_label": "dep_2026_09",
  "status": "pending",
  "created_at": "2026-09-15T10:00:00Z",
  "updated_at": "2026-09-15T10:30:00Z"
}
```

### 7. Bank Account & Bank Transaction
```json
// Bank Account
{
  "id": "18cfb704-5f5c-44bf-a9f4-18c9ecf83e58",
  "property_id": "31b6ea55-ec44-42b7-a3f1-f896b5fc24ee",
  "bank_name": "HDFC Bank",
  "account_type": "current",
  "account_number_last4": "4321",
  "label": "Primary Operations Account",
  "statement_profile": "hdfc",
  "is_active": true,
  "created_at": "2026-09-01T00:00:00Z",
  "updated_at": "2026-09-01T00:00:00Z"
}

// Bank Transaction
{
  "id": "39ecb815-6a6d-45cf-ba05-29d0fdf94f69",
  "property_id": "31b6ea55-ec44-42b7-a3f1-f896b5fc24ee",
  "account_id": "18cfb704-5f5c-44bf-a9f4-18c9ecf83e58",
  "txn_date": "2026-09-05",
  "value_date": "2026-09-05",
  "description": "UPI/424912345678/RENT/PG-RENT02",
  "ref_number": "424912345678",
  "txn_type": "credit",
  "amount_paise": 550000,
  "balance_paise": 12500000,
  "status": "matched",
  "matched_due_id": "d2b51417-e7bc-487f-b905-095511fb1a02",
  "matched_by": "76ba3f53-271d-4076-96ad-d00730d8ebc1",
  "matched_at": "2026-09-05T10:15:00Z"
}
```

### 8. Gateway Settlement & Multi-Way Daily Settlement Balance
```json
// Daily Settlement Balance Snapshot
{
  "id": "5a0dc926-7b7e-46df-cb16-30e1aef05a70",
  "property_id": "31b6ea55-ec44-42b7-a3f1-f896b5fc24ee",
  "balance_date": "2026-09-15",
  "gateway_gross_paise": 5000000,
  "gateway_fee_paise": 100000,
  "gateway_tax_paise": 18000,
  "gateway_net_paise": 4882000,
  "gateway_in_transit_paise": 0,
  "bank_opening_paise": 10000000,
  "bank_inflows_paise": 4882000,
  "bank_outflows_paise": 1175000,
  "bank_closing_paise": 13707000,
  "unapplied_receipts_paise": 0,
  "gl_bank_balance_paise": 13707000,
  "gl_clearing_balance_paise": 0,
  "gl_unapplied_balance_paise": 0,
  "clearing_variance_paise": 0,
  "bank_variance_paise": 0,
  "unapplied_variance_paise": 0,
  "status": "balanced",
  "computed_at": "2026-09-15T23:59:59Z"
}
```

### 9. Staff Profile & Daily Attendance Record
```json
// Staff Profile
{
  "id": "6b1ed037-8c8f-47ef-dc27-41f2bff16b81",
  "property_id": "31b6ea55-ec44-42b7-a3f1-f896b5fc24ee",
  "payee_id": "a9ed6286-5040-43a9-c9d0-0330630b1ef4",
  "name": "Sunil Kumar",
  "role": "Security Guard",
  "phone": "+919876543220",
  "base_monthly_wage_paise": 2400000,
  "effective_from": "2026-08-01T00:00:00Z",
  "status": "active",
  "created_at": "2026-08-01T00:00:00Z",
  "updated_at": "2026-08-01T00:00:00Z"
}

// Attendance Record
{
  "id": "7c2fe148-9d9a-48f0-ed38-5203c0027c92",
  "property_id": "31b6ea55-ec44-42b7-a3f1-f896b5fc24ee",
  "staff_id": "6b1ed037-8c8f-47ef-dc27-41f2bff16b81",
  "work_date": "2026-09-01T00:00:00Z",
  "status": "present",
  "recorded_by": "76ba3f53-271d-4076-96ad-d00730d8ebc1",
  "created_at": "2026-09-01T08:00:00Z",
  "updated_at": "2026-09-01T08:00:00Z"
}
```

### 10. Monthly Wage Calculation Snapshot
```json
{
  "id": "8d30f259-0e0b-4901-fe49-6314d1138da3",
  "property_id": "31b6ea55-ec44-42b7-a3f1-f896b5fc24ee",
  "staff_id": "6b1ed037-8c8f-47ef-dc27-41f2bff16b81",
  "cycle_month": "2026-09",
  "base_monthly_wage_paise": 2400000,
  "prorated_base_wage_paise": 2400000,
  "total_basis_days": 30,
  "employed_basis_days": 30,
  "days_present": 28.0,
  "days_paid_leave": 0.0,
  "days_holiday": 0.0,
  "days_absent": 2.0,
  "days_unrecorded": 0.0,
  "free_leave_days_allowed": 2.0,
  "excess_absent_days": 0.0,
  "per_day_rate_paise": 80000,
  "total_deduction_paise": 0,
  "net_wage_paise": 2400000,
  "payout_item_id": "cb0f84a8-7262-45cb-ebf2-2552852d3016",
  "status": "batched",
  "calculated_at": "2026-09-30T23:59:59Z",
  "finalized_by": "76ba3f53-271d-4076-96ad-d00730d8ebc1"
}
```

## Postman

Import `postman/pg-go.postman_collection.json` and `postman/pg-go.postman_environment.json`. Folders include Public, Join, Owner, Tenant, Search, Notifications, Preferences, Manager, Gamification, Legacy OTP.

1. Sign in with Firebase (Phone OTP or Google + linked phone) and copy the ID token
2. Set `id_token` (and `invite_code` for a new tenant phone)
3. Run **Firebase Exchange** — `token` and `tenant_id` auto-populate
4. Owners skip invite; pending tenants call Join endpoints until activate, then exchange again so the JWT includes `tenant_id`
5. Set `property_id` for facility / manager queries

## CORS

Set in `.env`:

```env
CORS_ALLOWED_ORIGINS=http://localhost:5173,http://127.0.0.1:5173
```

- Allowed headers: `Authorization`, `Content-Type`, `Idempotency-Key`, `Accept-Language`
- Allowed methods: `GET`, `POST`, `PATCH`, `OPTIONS`
- `AllowCredentials`: `false`
- `MaxAge`: `12 * time.Hour`

Required for pg-react local dev (`localhost:5173`). Production embedded PWA is served same-origin by pg-go and does not require CORS.
