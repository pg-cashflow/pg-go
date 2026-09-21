# API Contract (pg-go)

**Rev 10.** HTTP source of truth for [pg-react](https://github.com/pg-cashflow/pg-react). Route list matches `NewRouter` in `internal/api/router.go`. TypeScript field shapes live in pg-react `packages/types/index.ts` (mirror of Go domain types; if they diverge, fix types to match this API).

**Out of scope:** scheduled CLIs (`billing-cycle`, `reminder`, `cashfree-poll`, `financial-summary`, `gamification-cycle`, `search-reindex`) and env/runbooks — see [README.md](README.md).

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
- **List envelopes:** responses wrap arrays — `{ tenants }`, `{ dues }`, `{ payments }`, `{ events }`, `{ inspections }`, `{ hazards }`, `{ rewards }`, `{ notifications }`

## Error Code Registry

The backend emits additive machine-readable error codes across authenticated and public endpoints.
The full registry of domain-qualified error codes is documented below:

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
| `finance.duplicateRequest` | 409 | Finance | Duplicate idempotency key |
| `finance.idempotencyRequired` | 400 | Finance | Idempotency-Key header is required |
| `finance.invalidAmount` | 400 | Finance | Invalid expense or payment amount |
| `finance.invalidKind` | 400 | Finance | Invalid transaction kind |
| `finance.overpay` | 400 | Finance | Payment exceeds remaining balance |
| `finance.expenseNotPayable` | 400 | Finance | Expense cannot accept payment |
| `finance.policyExceeded` | 400 | Finance | Spend policy exceeded |
| `finance.approvalRequired` | 400 | Finance | Owner approval required before payment |
| `finance.periodNotCloseable` | 400 | Finance | Unexplained difference blocks period close |
| `finance.notFound` | 404 | Finance | Financial entity not found |
| `finance.forbidden` | 403 | Finance | Forbidden financial action |
| `finance.disabled` | 503 | Finance | Financial subsystem is disabled |
| `request.invalidBody` | 400 | Request | Request payload is malformed or missing required fields |
| `request.invalidId` | 400 | Request | UUID URL parameter is malformed |
| `request.dueDayInvalid` | 400 | Request | Due day must be an integer between 1 and 28 |
| `request.imageTooLarge` | 400 | Request | Uploaded image exceeds maximum allowed size (2MB) |
| `request.imageReadFailed` | 400 | Request | Server failed to read uploaded multipart file stream |
| `preferences.localeRequired` | 400 | Preferences | Locale field is required |
| `preferences.invalidLocale` | 400 | Preferences | Unsupported or invalid locale |
<!-- ERROR_CODES_END -->

## Role matrix

| Role | JWT | Route groups |
| ---- | --- | ------------ |
| owner | `role=owner`, `property_id` set | `/api/owner/*`, `/api/manager/*` (owners may call warden routes), `/api/search`, `/api/notifications`, `/api/me/preferences`, `POST /api/auth/revoke-sessions` |
| manager | `role=manager`, `property_id` set | `/api/manager/*` (mutating `property_id` must match claims), `/api/search`, `/api/notifications`, `/api/me/preferences`, `POST /api/auth/revoke-sessions` |
| tenant (onboarded) | `role=tenant`, `tenant_id` + `property_id`; live status `active` or `pending_allocation` | `/api/tenant/*`, `/api/search`, `/api/notifications`, `/api/me/preferences`, `POST /api/auth/revoke-sessions` |
| pending join | `role=tenant`, `tenant_id` null, `property_id` set | `/api/join/me`, `POST /api/join`, `/api/me/preferences` only. `/api/tenant/*` → 403 `"complete your profile to continue"` |

Never send `property_id` as a *security* claim from the client for owner/tenant scoped lists — owner/tenant money routes use JWT `property_id`. Some manager/owner **facility** handlers still take `property_id` as query/JSON; managers must match JWT property or get 403 `"manager not authorized for this property"`.

## Auth

Login is Firebase Phone OTP or Google (Gmail). There is **no Owner vs Tenant role picker**. Owners match via seeded `owner_phone` or `owner_email`. Invite code is how a new phone or Google login becomes a pending tenant.

**Primary login is Firebase.** Rent-reminder SMS still uses `SMS_*` / `internal/sms`.

| Method | Path                        | Body                             | Response                                                                                                                                                                                                                |
| ------ | --------------------------- | -------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| POST   | `/api/auth/firebase`        | `{ "id_token", "invite_code"? }` | `{ "token", "user": { id, phone?, email?, role, tenant_id?, property_id?, locale, has_saved_preference } }`                                                                                                                                           |
| POST   | `/api/auth/revoke-sessions` | (empty)                          | `{ "ok": true }` — **any authenticated role** (owner, manager, tenant). Increments `users.token_version` so this and all other sessions for that user fail with 403 `"access revoked"` on the next API call. Client should clear the stored token. |


| Status | When |
| ------ | ---- |
| 401    | Invalid or claims-less Firebase token; JWT missing (`"missing bearer token"`) or invalid (`"invalid token"`) |
| 403    | Tenant vacated (`"access revoked"`); Google email not verified; JWT `token_version` stale (`"access revoked"`); pending join hitting `/tenant/*` (`"complete your profile to continue"`); manager `property_id` mismatch (`"manager not authorized for this property"`) |
| 404    | Unknown account without a valid invite — `"Account not found. If you are an owner, verify your registered phone/email."` |
| 503    | Firebase Admin not configured |

Unknown phone + **valid invite** creates `users.role=tenant` with `tenant_id` null and a `join_requests` row. Until `POST /join` completes, `/tenant/*` returns 403 `"complete your profile to continue"`. After onboarding, the tenant is `pending_allocation` (dashboard allowed; pay disabled until `tenants.status=active`).

### Legacy OTP (rollback / not primary)

Still registered on the router. Prefer Firebase for product login.

| Method | Path                     | Body                    | Notes |
| ------ | ------------------------ | ----------------------- | ----- |
| POST   | `/api/auth/otp/request`  | `{ "phone" }`           | Per-IP rate limit 3/min (burst 5). `{ "ok": true }`. 429 `"rate limited"`. |
| POST   | `/api/auth/otp/verify`   | `{ "phone", "otp" }`    | Returns `{ "token", "user": { id, phone?, email?, role, tenant_id?, property_id?, locale, has_saved_preference } }` like Firebase exchange. |


> **Security Note on Multi-Provider Firebase UIDs and Session Lifetimes:**
> `users.firebase_uid` tracks the user's most-recently authenticated Firebase identity (Phone OTP or Google).
> Revoking a Firebase UID via the Firebase Admin SDK (`auth.RevokeRefreshTokens`) only invalidates Firebase refresh tokens, preventing that identity from minting new Firebase ID tokens to exchange at `POST /auth/firebase`.
>
> - **Tenants:** Revocation is enforced per-request via live database checks in `RequireTenant` (`tenants.status` must be `active` or `pending_allocation`). Marking a tenant `vacated` immediately revokes access on the next API call, regardless of JWT expiry. All roles also carry `token_version` in the JWT, checked against `users.token_version`.
> - **All roles:** `POST /api/auth/revoke-sessions` increments `users.token_version`. Subsequent requests with older JWTs return 403 `"access revoked"`. Firebase UID revocation still only blocks minting new Firebase ID tokens for `POST /auth/firebase`.
>
> **Security Note on Email Verification (Google / Gmail):**
> `POST /auth/firebase` strictly enforces `email_verified: true` in the cryptographically verified Firebase ID token claims for Google logins (returning HTTP 403 `ErrEmailNotVerified` if unverified or absent). In Google OIDC federation, this provides cryptographic proof of mailbox possession equivalent in trust to SMS Phone OTP, preventing client-asserted or spoofed email addresses from accessing Owner accounts.

## Join (invite-in, dashboard-now)

The invite code **is** authorization. There is no second owner identity review. Owner only assigns room/rent later.

| Method | Path                                    | Auth               | Notes |
| ------ | --------------------------------------- | ------------------ | ----- |
| GET    | `/api/join/invite/:code`                | public             | `{ property_id, property_name, owner_name }` — never VPA |
| GET    | `/api/join/me`                          | pending tenant JWT | `{ join, message }` — only while profile not yet submitted (`tenant_id` null) |
| POST   | `/api/join`                             | pending tenant JWT | **multipart** preferred: `name`, `permanent_address`, `current_address`, `parent_name`, `emergency_phone`, `consent=true`, file `image` (ID photo ≤2MB, no OCR). Creates `pending_allocation` tenant, links `users.tenant_id`, marks join `approved`. Response `{ join, tenant, message }`. Client must re-exchange Firebase token for JWT with `tenant_id`. |
| GET    | `/api/owner/invite`                     | owner              | `{ invite_code, payment_mode }` |
| POST   | `/api/owner/invite/rotate`              | owner              | `{ invite_code }` |
| GET    | `/api/owner/join-requests`              | owner              | Query `status` → `{ join_requests }`. Use `approved` for awaiting room/rent; `pending` for incomplete profiles (rejectable). |
| POST   | `/api/owner/join-requests/:id/activate` | owner              | **Assign terms**: `{ room_number?, rent_amount, due_day, deposit_amount? }` — flips tenant to `active`, creates deposit due |
| POST   | `/api/owner/join-requests/:id/reject`   | owner              | Only while join `status=pending` (never finished form). `{ ok: true }` |
| GET    | `/api/owner/tenants/:id/id-photo`       | owner              | Raw image bytes. Never in list JSON (`has_id_photo` flag only). |

Tenant profile fields: `permanent_address`, `current_address`, `parent_name`, `emergency_phone`, `joined_on` (server date of submit). Personal phone is the login phone.

Walk-in `POST /api/owner/tenants` remains for phone-less / cash-only people. Do not use it as the default onboarding path.

Events: `JoinRequested`, `JoinApproved`, `JoinRejected`, `TenantCreated` (pending_allocation), `ConsentGiven` (id photo), `DepositTermsAccepted` on assign-terms.

## Owner (role=owner)

| Method | Path                                     | Notes |
| ------ | ---------------------------------------- | ----- |
| GET    | `/api/owner/properties`                  | `{ properties: [...] }` — UPI VPA **never** returned |
| GET    | `/api/owner/tenants`                     | `{ tenants: [...] }` — includes profile fields + `has_id_photo` |
| POST   | `/api/owner/tenants`                     | `{ name, phone?, room_number?, rent_amount, due_day, notice_period_days?, deposit_amount? }` |
| PATCH  | `/api/owner/tenants/:id`                 | Partial update |
| GET    | `/api/owner/tenants/:id/id-photo`        | Raw ID photo bytes |
| POST   | `/api/owner/tenants/:id/notice`          | Optional `{ notice_given_at }` |
| POST   | `/api/owner/tenants/:id/vacate`          | |
| POST   | `/api/owner/tenants/:id/attach-phone`    | `{ phone }` |
| POST   | `/api/owner/tenants/:id/prorate`         | `{ vacate_date }` |
| POST   | `/api/owner/tenants/:id/deposit/settle`  | `{ refunded_amount_paise, reason? }` |
| GET    | `/api/owner/dues`                        | Query: `tenant_id`, `kind` (`rent` \| `deposit` \| `electricity` \| `water`), `status` → `{ dues: [...] }` |
| POST   | `/api/owner/dues/:id/waive`              | Returns updated due |
| POST   | `/api/owner/dues/:id/match`              | `{ amount, upi_txn_id }` |
| POST   | `/api/owner/dues/:id/mark-cash-paid`     | `{ amount, note? }` — all-or-nothing |
| GET    | `/api/owner/dues/:id/qr`                 | PNG image |
| GET    | `/api/owner/dues/:id/pay`                | JSON pay payload (see below) |
| POST   | `/api/owner/dues/:id/token`              | `{ path, url, wa_me? }` |
| GET    | `/api/owner/payment-reports`             | Query `status` → `{ payment_reports }` |
| POST   | `/api/owner/payment-reports/:id/confirm` | Owner confirm → existing ManualMatch |
| POST   | `/api/owner/payment-reports/:id/reject`  | Optional `{ note }` |
| GET    | `/api/owner/payments`                    | Query: `matched_by` → `{ payments: [...] }` |
| POST   | `/api/owner/statements/import`           | multipart `file` (CSV) |
| GET    | `/api/owner/reconciliation`              | Query: `period=YYYY-MM` → summary object |
| GET    | `/api/owner/events`                      | Query: `tenant_id`, `type`, `from`, `to` (`YYYY-MM-DD`), `limit` → `{ events: [...] }` |

### Facility & gamification (owner)

Field-level shapes: `internal/domain/gamification.go` / `@pg/types`.

| Method | Path                                  | Notes |
| ------ | ------------------------------------- | ----- |
| GET    | `/api/owner/gamification/settings`    | Query `property_id` (required). `{ settings, rules }` |
| PATCH  | `/api/owner/gamification/settings`    | JSON body `PropertyGamificationSettings` (include `property_id`). `{ settings }` |
| GET    | `/api/owner/floors`                   | Query `property_id` (required). `{ floors }` |
| POST   | `/api/owner/floors`                   | `{ property_id, floor_number, name }` → `{ floor }` |
| GET    | `/api/owner/rooms`                    | Query `property_id` (required). `{ rooms }` |
| POST   | `/api/owner/rooms`                    | `{ property_id, floor_id, room_number, capacity, included_units }` → `{ room }` |
| POST   | `/api/owner/managers`                 | `{ phone, property_id? }` — provisions `users.role=manager`. Defaults to claims `property_id`. `{ manager }` |

### ReconciliationSummary

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

## Manager / warden (`RequireManagerOrOwner`)

Owners may call these. Managers must use their JWT property; mismatch → 403. Full item schemas: `@pg/types` / `internal/domain/gamification.go`.

| Method | Path                                       | Notes |
| ------ | ------------------------------------------ | ----- |
| POST   | `/api/manager/inspections`                 | `{ property_id, room_id?, floor_id?, inspection_type, notes, items[] }` — each item: `item_key`, `description`, `passed`, `photo_base64?`, `notes`. `{ inspection }` |
| GET    | `/api/manager/inspections`                 | Query `property_id` (defaults from JWT). `{ inspections }` |
| POST   | `/api/manager/inspections/items/:id/resolve` | `{ status: "upheld" \| "overturned" }` |
| POST   | `/api/manager/violations`                  | `{ tenant_id, rule_code, severity, description, evidence_base64? }` — `severity`: `safety` \| `lifestyle`. `{ violation }` |
| POST   | `/api/manager/meter-readings`              | `{ property_id, kind, reading_value, room_id?, floor_id?, meter_replaced?, confirm_anomaly? }` — `kind`: `electricity` \| `water`. `{ result }` |
| GET    | `/api/manager/kitchen/headcount`           | Query `property_id` (defaults from JWT), `date?` (`YYYY-MM-DD`). `{ headcount }` |
| GET    | `/api/manager/hazards`                     | Query `property_id` (defaults from JWT), `status?`. `{ hazards }` |
| POST   | `/api/manager/hazards/:id/resolve`         | `{ status: "resolved" \| "rejected" }` — defaults to `resolved` if omitted |
| POST   | `/api/manager/vendor-inspections`          | `{ property_id, vendor_name, score_percent, notes?, penalty_paise?, photo_base64? }` → `{ vendor_inspection }` |

## Tenant (`active` or `pending_allocation`)

`RequireTenant` allows `active` and `pending_allocation`. **Pay / QR / Cashfree / UTR reports** require `tenants.status=active` (`IsPayable()`). Pending-allocation tenants can load dashboard, points, community, and profile.

### Ledger & KYC

| Method | Path                           | Notes |
| ------ | ------------------------------ | ----- |
| GET    | `/api/tenant/me`               | Tenant object + `active_dues[]` |
| GET    | `/api/tenant/dues`             | `{ dues: [...] }` |
| GET    | `/api/tenant/dues/:id/qr`      | PNG (own due only); 409 in Cashfree mode |
| GET    | `/api/tenant/dues/:id/pay`     | JSON pay payload (see below). `payable` false if due paid/waived **or** tenant not `active` |
| POST   | `/api/tenant/dues/:id/reports` | `{ upi_txn_id, amount?, note? }` or multipart (`image` ≤2MB) |
| GET    | `/api/tenant/payments`         | `{ payments: [...] }` |
| POST   | `/api/tenant/push/subscribe`   | `{ endpoint, keys: { p256dh, auth } }` |
| POST   | `/api/tenant/aadhaar`          | `{ consent: true, qr_payload?, uid_last4?, confirm? }` — Secure QR verify; last-4 only. Without `confirm`, returns decoded fields for user review. |

### Points, community, inspections

| Method | Path                                       | Notes |
| ------ | ------------------------------------------ | ----- |
| GET    | `/api/tenant/points`                       | `{ balance, expiring_soon, earliest_expiry, on_time_months, freezes_left, ledger }` |
| GET    | `/api/tenant/rewards`                      | `{ rewards }` — each catalog item plus `eligible`, `reason?` |
| POST   | `/api/tenant/rewards/:id/redeem`           | `{ redemption }` |
| GET    | `/api/tenant/inspections`                  | `{ inspections }` (with items) |
| POST   | `/api/tenant/inspections/items/:id/dispute` | `{ dispute_note }` (required). `{ status: "disputed" }` |
| GET    | `/api/tenant/meal-rsvp`                    | Tomorrow UTC: `{ date, breakfast, lunch, dinner }` |
| POST   | `/api/tenant/meal-rsvp`                    | `{ date: "YYYY-MM-DD", slot: "breakfast"\|"lunch"\|"dinner", attending }` → `{ rsvp }` |
| GET    | `/api/tenant/menu-poll`                    | `{ poll, votes }` or `{ poll: null }` |
| POST   | `/api/tenant/menu-poll/vote`               | `{ poll_id, option_id }` → `{ status: "voted" }` |
| POST   | `/api/tenant/hazards`                      | multipart (`category`, `description`, `image?`) or JSON `{ category, description, photo_base64? }` |
| GET    | `/api/tenant/violations`                   | `{ violations }` — own tenant only |
| GET    | `/api/tenant/leaderboard`                  | `{ streaks, floor_scores }` |
| GET    | `/api/tenant/referrals`                    | `{ referrals }` |
| POST   | `/api/tenant/referrals`                    | `{ phone, name? }` — `phone` required |

## Pay JSON

`GET /api/{owner|tenant}/dues/:id/pay` (VPA is **only** on this payload, never on `GET /api/owner/properties`):

```json
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
```

`payable` is false when the due is `paid` or `waived` — hide Pay / save / copy. `mode=cashfree` only when `properties.payment_mode=cashfree` **and** Cashfree keys are configured. While `payment_mode=manual` (default), no Cashfree orders are created even if keys exist. Owner absorbs TDR; tenant `order_amount` equals remaining due; checkout is **UPI-only**; do not enable surcharge/convenience-fee.

When `mode=cashfree`, PNG QR routes return **409** (personal VPA QR is replaced, not shown beside checkout). Use `/pay` JSON `payment_session_id` or the magic-link Cashfree button.

`GET /p/:token` HTML: manual mode = Save QR / Copy UPI ID / Copy `PG-XXXXXX` / Open UPI. Cashfree mode = Pay with UPI checkout (no personal VPA). Hide when paid/waived.

CSV import, owner `match`, and `mark-cash-paid` remain fallbacks after a Cashfree flip.

## Cashfree webhook and poll (dormant)

| Method | Path                 | Notes |
| ------ | -------------------- | ----- |
| POST   | `/webhooks/cashfree` | Raw body HMAC (`x-webhook-signature` = Base64(HMAC-SHA256(timestamp+rawBody, webhook secret)), `x-webhook-timestamp`). `CASHFREE_WEBHOOK_SECRET` defaults to `CASHFREE_SECRET_KEY`. Production with Cashfree enabled fails closed if the secret is still empty. Settles `PAYMENT_SUCCESS_WEBHOOK` only when `payment_amount` matches `payment_intents.amount_paise`, via `settleMatched` with `matched_by=cashfree`. Idempotent on `cf_payment_id` and `payments.upi_txn_id`. `MarkPaid` only after settle or duplicate UTR. Unknown order → 200; DB errors → 500. Failed/dropped types return 200 and do not change the due. Do not call Cashfree from this handler. |

Missed-webhook poll: `go run ./cmd/cashfree-poll/` — intents `created` older than 10 minutes whose due is still `pending`/`partial`. No-op without `CASHFREE_APP_ID` / `CASHFREE_SECRET_KEY`.

Flip live only after KYC: production keys, then `UPDATE properties SET payment_mode='cashfree'`. Sandbox keys + `CASHFREE_ENV=sandbox` until then.

D+1/D+7 reminders for `payment_mode=cashfree` require a payment-intent for that due with `updated_at` in the last 24h (poll touches on every fetch), or no intents yet (tenant never started checkout). Manual mode still requires a CSV import in the last 24h.

## Payment reports (UTR proof)

Unique `upi_txn_id` on `payment_reports` and `payments`. Duplicate UTR → 409. Already paid/waived due → 409 `"already recorded"`. Owner confirm reuses ManualMatch. Cash is **not** this path — owner `mark-cash-paid` only. OCR sidecar is v1.1 (not in this API).

Events: `PaymentReportSubmitted`, `PaymentReportRejected`.

## Global search (RBAC)

Scoped by JWT role and `property_id` / `tenant_id` from claims — never accept `property_id` from the client.

| Method | Path          | Auth | Query params |
| ------ | ------------- | ---- | ------------ |
| GET    | `/api/search` | owner, manager, or tenant (valid session) | `q` (required), `limit` (default 20, max 50), `types` (optional comma filter), `mode` (`lexical` default, `hybrid` when semantic index is populated) |

**Query rules:** trimmed `q`; minimum 2 characters unless `q` looks like a due code / UTR token (6+ alphanumeric/hyphen). Rate-limited per IP (30/min sustained, burst 10).

**Hybrid ops:** `mode=hybrid` needs migrations `010_search_lexical.sql` and `011_search_semantic.sql` (pgvector), `SEARCH_EMBEDDING=hash` (dev) or a real embedder, then `go run ./cmd/search-reindex/`. Without embeddings, the API falls back to lexical.

**Response:**

```json
{
  "q": "ravi",
  "mode": "lexical",
  "results": [
    {
      "type": "tenant",
      "id": "uuid",
      "title": "Ravi K",
      "subtitle": "Room 12 · active",
      "path": "/owner/tenants?q=ravi"
    }
  ]
}
```

| Role    | Entity types returned |
| ------- | --------------------- |
| owner   | `tenant`, `due`, `payment`, `payment_report`, `join_request`, `event`, `document` (hybrid) |
| manager | `tenant`, `inspection`, `hazard`, `violation`, `document` (hybrid) |
| tenant  | `due`, `payment`, `document` (hybrid) — scoped to `claims.tenant_id` only |

`path` is a pg-react deep link (client route + query). Navigation items are client-only (command palette); not returned by this API.

## Notifications

Scoped to claims.UserID at the repo layer. All roles (owner, manager, tenant) share the same endpoints.

| Method | Path                          | Auth | Notes |
| ------ | ----------------------------- | ---- | ----- |
| GET    | `/api/notifications`          | any  | Query `cursor`, `limit` (1–50, default 20). Returns `{ notifications, unread_count, next_cursor? }` |
| PATCH  | `/api/notifications/:id/read` | any  | Mark single notification as read |
| PATCH  | `/api/notifications/read-all` | any  | Mark all notifications as read |

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

- `is_action_required` (boolean): `true` if the notification represents an actionable pending item demanding immediate user intervention (e.g. pending expense approval, Level-3 tie-out exception, payment report review, inspection dispute). Used in pg-react to render persistent alert badges and primary action buttons.

## Preferences & Locales

The localization system coordinates multi-lingual support across `pg-go` and `pg-react`. Supported locales are validated against a strict platform registry and stored per user.

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
| GET | `/api/me/preferences` | Any authenticated role (`owner`, `manager`, `tenant`, `pending join`) | `Authorization: Bearer <jwt>`, optional `Accept-Language` | None | `{ "locale": "en-IN", "has_saved_preference": boolean }` |
| PATCH | `/api/me/preferences` | Any authenticated role (`owner`, `manager`, `tenant`, `pending join`) | `Authorization: Bearer <jwt>` | `{ "locale": "te-IN" }` | `{ "locale": "te-IN", "has_saved_preference": true }` |

### Detailed Behavior & Error Contracts

#### 1. `GET /api/locales`
Returns the list of enabled platform locales and the default fallback tag (`en-IN`). Used by client language pickers and onboarding flows.

```json
{
  "locales": [
    { "code": "en-IN", "name": "English", "native_name": "English" },
    { "code": "te-IN", "name": "Telugu", "native_name": "తెలుగు" },
    { "code": "ta-IN", "name": "Tamil", "native_name": "தமிழ்" },
    { "code": "kn-IN", "name": "Kannada", "native_name": "ಕನ್ನಡ" }
  ],
  "default_locale": "en-IN"
}
```

#### 2. `GET /api/me/preferences`
Retrieves the active user's saved preference from the `user_preferences` table.
- If the user has explicitly saved a preference: returns `{ "locale": "<saved-code>", "has_saved_preference": true }`.
- If no saved preference exists (`pgx.ErrNoRows`) or preferences store is unconfigured: resolves the locale from the incoming request's `Accept-Language` header (matching against supported BCP-47 tags, defaulting to `en-IN`) and returns `{ "locale": "<resolved-code>", "has_saved_preference": false }`.

```json
{
  "locale": "en-IN",
  "has_saved_preference": false
}
```

#### 3. `PATCH /api/me/preferences`
Updates or inserts (`ON CONFLICT (user_id) DO UPDATE`) the user's preferred language tag in `user_preferences`.

Request body:
```json
{
  "locale": "te-IN"
}
```

Success response (200 OK):
```json
{
  "locale": "te-IN",
  "has_saved_preference": true
}
```

Validation & error responses:
| Status | Body | When |
| :--- | :--- | :--- |
| 400 | `{ "error": "locale is required" }` | Missing or empty `locale` field in JSON payload (Gin binding validation). |
| 400 | `{ "error": "unsupported locale", "code": "preferences.invalidLocale" }` | Provided tag is not in `localization.SupportedLocales` catalog. |
| 400 | `{ "error": "invalid locale", "code": "preferences.invalidLocale" }` | Postgres `23514` check constraint violation (`ErrInvalidLocale`). |
| 401 | `{ "error": "missing bearer token" }` / `{ "error": "invalid token" }` | Missing or invalid JWT. |
| 500 | `{ "error": "internal error" }` | Preferences repository not configured on backend (no code field). |

### Error Code Registry

Machine-readable error identifiers emitted in the `{ "error": "...", "code": "..." }` response body for client-side localization and interceptor branching.

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
| **Finance** | `finance.duplicateRequest` | 409 | Mutating financial request with duplicate Idempotency-Key |
| **Finance** | `finance.idempotencyRequired` | 400 | Idempotency-Key header is missing on mutating financial endpoint |
| **Finance** | `finance.invalidAmount` | 400 | Financial amount in paise must be positive integer |
| **Finance** | `finance.invalidKind` | 400 | Invalid capital transaction or payment kind |
| **Finance** | `finance.overpay` | 400 | Expense payment amount exceeds remaining balance |
| **Finance** | `finance.expenseNotPayable` | 400 | Expense is pending maker-checker approval or already paid |
| **Finance** | `finance.policyExceeded` | 400 | Expense exceeds manager discretionary spending policy limit |
| **Finance** | `finance.approvalRequired` | 400 | Expense requires owner approval before disbursement |
| **Finance** | `finance.periodNotCloseable` | 400 | Period tie-out has unexplained variance and cannot close |
| **Finance** | `finance.notFound` | 404 | Financial transaction, budget, or expense record not found |
| **Finance** | `finance.forbidden` | 403 | User does not have authorization for this financial mutation |
| **Finance** | `finance.disabled` | 503 | Financial intelligence engine is disabled in property settings |
| **Request** | `request.invalidBody` | 400 | Request JSON payload malformed or missing required fields |
| **Request** | `request.invalidId` | 400 | UUID URL parameter is malformed |
| **Request** | `request.dueDayInvalid` | 400 | Due day must be an integer between 1 and 28 |
| **Request** | `request.imageTooLarge` | 400 | Uploaded image exceeds maximum allowed size (2MB) |
| **Request** | `request.imageReadFailed` | 400 | Server failed to read uploaded multipart file stream |
| **Preferences** | `preferences.localeRequired` | 400 | Missing locale field in preferences payload |
| **Preferences** | `preferences.invalidLocale` | 400 | Locale tag not supported or failed database check constraint |
<!-- ERROR_CODES_DETAIL_END -->

### Localization Middleware & Request Context
All HTTP requests pass through `localization.Middleware()`:
1. Determines locale based on:
   - Authenticated user's explicit preference in `user_preferences` (if present).
   - HTTP `Accept-Language` header matched via `golang.org/x/text/language.NewMatcher` (supports regional/dialect fallbacks, e.g. `te` or `te-US` $\rightarrow$ `te-IN`).
   - Platform default fallback: `en-IN`.
2. Binds resolved locale into request context via `localization.WithLocale(ctx, locale)`. Handlers access this via `localization.LocaleFromContext(ctx)`.

### Client Synchronization & Concurrency Discipline (pg-react)
1. **Initial Hydration & Sync**:
   - Authenticated session exchange (`POST /api/auth/firebase` or `POST /api/auth/otp/verify`) returns `{ "locale", "has_saved_preference" }` inside the `user` payload.
   - If `has_saved_preference: true`, client synchronizes local storage to match the server's persisted preference.
   - If `has_saved_preference: false`, client adopts browser/stored locale and issues a non-blocking background `PATCH /api/me/preferences` to save the initial choice.
2. **Race Condition & Abort Guards**:
   - `updateMyPreferences` takes an optional `AbortSignal`. Rapid user language toggles cancel prior in-flight requests.
   - Monotonic request sequence tracking ensures that out-of-order network responses never overwrite newer selections (last-write-wins).
3. **Resilience & Offline Retries**:
   - Failed preference sync attempts do not block UI interaction.
   - Uncommitted preference updates automatically retry when network connectivity restores (`window.addEventListener('online', ...)`) or window regains focus (`window.addEventListener('focus', ...)`).

## Finance, ROI & Intelligence (Rev 8)

All financial amounts are in **bigint paise** (integer).

### Idempotency Discipline
All mutating financial endpoints require an `Idempotency-Key` header.
- Header: `Idempotency-Key: <unique-client-key>`
- If missing: `400 Bad Request` (`"Idempotency-Key required"`)
- If duplicate within property: `409 Conflict` (`"duplicate request"`)

### Owner Finance Routes

| Method | Path | Body / Query | Response / Notes |
| :--- | :--- | :--- | :--- |
| GET | `/api/owner/finance/summary` | Query: `period=YYYY-MM` | `{ period, collections: ReconciliationSummary, operating: OperatingSummary, query_keys, invalidate_with }`. Explicit labels: `"Collections (reconciliation)"` vs `"Operating view (ledger)"`. |
| GET | `/api/owner/finance/ledger` | Query: `period`, `from`, `to`, `account` | `{ entries: JournalLine[] }`. Balanced double-entry lines. |
| POST | `/api/owner/finance/capital` | `{ kind: "initial"\|"additional"\|"withdrawal", amount_paise, purpose }` | `{ capital: CapitalTransaction }`. Header: `Idempotency-Key`. Equity transaction. |
| GET | `/api/owner/finance/capital` | - | `{ capital: CapitalTransaction[] }`. |
| POST | `/api/owner/finance/expenses` | `{ category_code, vendor_name, description, amount_paise, emergency?, room_id? }` | `{ expense: Expense, approval?: ApprovalRequest }`. Header: `Idempotency-Key`. |
| GET | `/api/owner/finance/expenses` | - | `{ expenses: Expense[] }`. |
| POST | `/api/owner/finance/expenses/:id/payments` | `{ amount_paise, method: "cash"\|"upi"\|"bank" }` | `{ payment: ExpensePayment }`. Header: `Idempotency-Key`. Partial payments allowed. |
| GET | `/api/owner/finance/advances` | - | `{ advances: ManagerAdvance[], outstanding_paise: int64 }`. Out-of-pocket manager advances. |
| POST | `/api/owner/finance/reimbursements` | `{ manager_user_id, amount_paise }` | `{ reimbursement: ManagerReimbursement }`. Header: `Idempotency-Key`. Reimburses manager advances. |
| GET | `/api/owner/finance/budgets` | Query: `period=YYYY-MM` | `{ budgets: Budget[] }`. |
| POST | `/api/owner/finance/budgets` | `{ category_code, period_month, amount_paise }` | `{ budget: Budget }`. Upsert monthly budget. |
| PATCH | `/api/owner/finance/budgets/:id` | Same as POST | `{ budget: Budget }`. |
| GET | `/api/owner/finance/settings` | - | `{ settings: PropertyFinanceSettings, policy: ApprovalPolicy, loyalty: PropertyGamificationSettings }`. Merged financial knobs. |
| PATCH | `/api/owner/finance/settings` | `{ settings?, policy?, loyalty? }` | `{ settings, policy, loyalty? }`. **Atomic single-DB transaction** update across all tables. |
| GET | `/api/owner/finance/approvals` | Query: `status=pending` | `{ approvals: ApprovalRequest[] }`. Maker-checker expense review queue. |
| POST | `/api/owner/finance/approvals/:id/:action` | `{ note? }` (`action` is `approve` or `reject`) | `{ ok: true }`. Approves or rejects expense/advance/budget. |
| GET | `/api/owner/finance/tie-out` | Query: `period=YYYY-MM` | `{ tie_out: PeriodTieOut, label: "Collections tie-out" }`. Reconciles collections control to ledger collection lines. |
| POST | `/api/owner/finance/tie-out/close` | Query: `period=YYYY-MM` | `{ tie_out: PeriodTieOut }`. Blocks with `400/422` if unexplained difference `!= 0`. |
| GET | `/api/owner/finance/variance-bridge` | Query: `period=YYYY-MM` | `{ variance_bridge: VarianceBridge, label: "Plan vs actual" }`. Decomposes budget vs actual OCF. |
| GET | `/api/owner/finance/imports` | - | `{ suggestions: ExpenseImportSuggestion[] }`. Unmatched debit CSV suggestions. |

### Owner ROI & Intelligence Routes

| Method | Path | Body / Query | Response / Notes |
| :--- | :--- | :--- | :--- |
| GET | `/api/owner/roi` | Query: `period=YYYY-MM` | `{ roi: { period, occupancy, operating, break_even, recovery, official } }`. `official: true` only after tie-out closed. |
| GET | `/api/owner/roi/recovery` | Query: `period=YYYY-MM` | Same as `/api/owner/roi`. |
| GET | `/api/owner/roi/break-even` | Query: `period=YYYY-MM` | Same as `/api/owner/roi`. |
| POST | `/api/owner/roi/scenario` | `{ current_occupancy_bps, target_occupancy_bps, capacity_beds, rent_per_bed_paise, fixed_opex_paise, variable_opex_per_bed_paise }` | `{ scenario: ScenarioResult }`. What-if break-even simulator. |
| GET | `/api/owner/insights` | - | `{ leakage: LeakageEvent[], recommendations: Recommendation[], coi_6m_paise, query_keys }`. |
| GET | `/api/owner/leakage` | - | Same as `/insights`. |
| GET | `/api/owner/leakage/:id` | - | `{ leakage: LeakageEvent }`. |
| GET | `/api/owner/recommendations` | - | `{ recommendations: Recommendation[] }`. |
| POST | `/api/owner/recommendations/:id/:action` | `{ realized_savings_paise? }` (`action` is `accept`\|`reject`\|`complete`) | `{ recommendation: Recommendation }`. |
| GET | `/api/owner/coi` | - | Same as `/insights`. Cost of Inaction. |
| GET | `/api/owner/forecast` | - | `{ forecast_ocf_paise: int64[], horizon_months: 12 }`. OCF forward forecast. |

### Manager Finance Routes

| Method | Path | Body / Query | Response / Notes |
| :--- | :--- | :--- | :--- |
| GET | `/api/manager/finance/expenses` | - | `{ expenses: Expense[] }`. Scoped to manager property. |
| POST | `/api/manager/finance/expenses` | `{ category_code, vendor_name, description, amount_paise, emergency?, room_id? }` | `{ expense, approval? }`. Checked against `approval_policies` in DB. Header: `Idempotency-Key`. |
| POST | `/api/manager/finance/expenses/:id/payments` | `{ amount_paise, method }` | `{ payment }`. Out-of-pocket manager payment recorded as `manager_advances`. Header: `Idempotency-Key`. |
| GET | `/api/manager/finance/today` | - | `{ expenses, pending_approvals, kitchen_headcount, hide_capital }`. Operational finance dashboard. |
| POST | `/api/manager/finance/meal-prep` | `{ meal_date, meal_slot: "breakfast"\|"lunch"\|"dinner", prepared_count, discarded_count? }` | `{ prep: MealPrepActual }`. Actuals for food leakage detector. |

### pg-react Query Key Succession & Invalidation Rules

When financial mutations succeed in pg-react:
1. **Collections & Dues**: Invalidate `['owner', 'reconciliation']`, `['owner', 'finance', 'summary']`, `['owner', 'finance', 'tie-out']`, `['owner', 'roi']`.
2. **Expenses & Payments**: Invalidate `['owner', 'finance', 'summary']`, `['owner', 'finance', 'expenses']`, `['owner', 'finance', 'advances']`, `['owner', 'finance', 'variance-bridge']`, `['owner', 'roi']`.
3. **Reimbursements**: Invalidate `['owner', 'finance', 'summary']`, `['owner', 'finance', 'advances']`, `['owner', 'finance', 'ledger']`.
4. **Tie-Out Close**: Invalidate `['owner', 'finance', 'tie-out']`, `['owner', 'roi']`.

## Public

| Method | Path                         | Notes |
| ------ | ---------------------------- | ----- |
| GET    | `/healthz`                   | `{ "status": "ok" }` |
| GET    | `/api/healthz`               | `{ "status": "ok" }` |
| GET    | `/api/locales`               | Supported locale registry: `{ locales, default_locale }` |
| GET    | `/api/push/vapid-public-key` | `{ "public_key": "..." }` |
| GET    | `/p/:token`                  | HTML payment page (save QR, copy VPA/note, open UPI) |
| POST   | `/p/:token/push/subscribe`   | Push from payment page |

## Postman

Import `postman/pg-go.postman_collection.json` and `postman/pg-go.postman_environment.json` (CONTRACT Rev 9). Folders include Public, Join, Owner, Tenant, Search, Notifications, Preferences, Manager, Gamification, Legacy OTP.

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
