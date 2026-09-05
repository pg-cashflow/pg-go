# API Contract (pg-go)

Source of truth for [pg-react](https://github.com/your-org/pg-react) integration.

## Base URL

- Local: `http://localhost:8080`
- Frontend env: `VITE_API_BASE_URL`

## Conventions

- **Money:** all amounts in **paise** (integer). ₹15,000 = `1500000`.
- **Errors:** `{ "error": "message" }`
- **Auth:** Firebase ID token exchanged at `POST /auth/firebase` for a 30-day app JWT. Subsequent calls use `Authorization: Bearer <jwt>`.
- **List envelopes:** responses wrap arrays — `{ tenants }`, `{ dues }`, `{ payments }`, `{ events }`

## Auth

Login is Firebase Phone OTP or Google (Gmail). There is **no Owner vs Tenant role picker**. Owners match via seeded `owner_phone` or `owner_email`. Invite code is how a new phone or Google login becomes a pending tenant.

| Method | Path             | Body                             | Response                                                                    |
| ------ | ---------------- | -------------------------------- | --------------------------------------------------------------------------- |
| POST   | `/auth/firebase` | `{ "id_token", "invite_code"? }` | `{ "token", "user": { id, phone?, email?, role, tenant_id?, property_id? } }` |

| Status | When                                                                                                                    |
| ------ | ----------------------------------------------------------------------------------------------------------------------- |
| 401    | Invalid or claims-less Firebase token                                                                                   |
| 403    | Tenant vacated, or Google email not verified                                                                            |
| 404    | Unknown account without a valid invite — `"Account not found. If you are an owner, verify your registered phone/email."` |
| 503    | Firebase Admin not configured                                                                                           |

Unknown phone + **valid invite** creates `users.role=tenant` with `tenant_id` null and a `join_requests` row. Until `POST /join` completes, `/tenant/*` returns 403 `"complete your profile to continue"`. After onboarding, the tenant is `pending_allocation` (dashboard allowed; pay disabled) until the owner assigns room/rent.

Rent-reminder SMS still uses `SMS_*` / `internal/sms`. Login OTP is not sent by this API.

> **Security Note on Multi-Provider Firebase UIDs and Session Lifetimes:**
> `users.firebase_uid` tracks the user's most-recently authenticated Firebase identity (Phone OTP or Google).
> Revoking a Firebase UID via the Firebase Admin SDK (`auth.RevokeRefreshTokens`) only invalidates Firebase refresh tokens, preventing that identity from minting new Firebase ID tokens to exchange at `POST /auth/firebase`.
> - **Tenants:** Revocation is enforced per-request via live database checks in `RequireTenant` (`tenants.status` must be `active` or `pending_allocation`). Marking a tenant `vacated` immediately revokes access on the next API call, regardless of JWT expiry.
> - **Owners:** There is currently no session-revocation mechanism for Owner accounts. A compromised Owner JWT remains valid until natural expiry (30 days), regardless of Firebase-side UID revocation. Immediate invalidation of Owner sessions currently requires rotating the global `JWTSecret` or adding a dedicated per-request `revoked_at` / token-blacklist mechanism.
>
> **Security Note on Email Verification (Google / Gmail):**
> `POST /auth/firebase` strictly enforces `email_verified: true` in the cryptographically verified Firebase ID token claims for Google logins (returning HTTP 403 `ErrEmailNotVerified` if unverified or absent). In Google OIDC federation, this provides cryptographic proof of mailbox possession equivalent in trust to SMS Phone OTP, preventing client-asserted or spoofed email addresses from accessing Owner accounts.


## Join (invite-in, dashboard-now)

The invite code **is** authorization. There is no second owner identity review. Owner only assigns room/rent later.

| Method | Path                                | Auth               | Notes |
| ------ | ----------------------------------- | ------------------ | ----- |
| GET    | `/join/invite/:code`                | public             | `{ property_id, property_name, owner_name }` — never VPA |
| GET    | `/join/me`                          | pending tenant JWT | `{ join, message }` — only while profile not yet submitted (`tenant_id` null) |
| POST   | `/join`                             | pending tenant JWT | **multipart** preferred: `name`, `permanent_address`, `current_address`, `parent_name`, `emergency_phone`, `consent=true`, file `image` (ID photo ≤2MB, no OCR). Creates `pending_allocation` tenant, links `users.tenant_id`, marks join `approved`. Response `{ join, tenant, message }`. Client must re-exchange Firebase token for JWT with `tenant_id`. |
| GET    | `/owner/invite`                     | owner              | `{ invite_code, payment_mode }` |
| POST   | `/owner/invite/rotate`              | owner              | `{ invite_code }` |
| GET    | `/owner/join-requests`              | owner              | Query `status` → `{ join_requests }`. Use `approved` for awaiting room/rent; `pending` for incomplete profiles (rejectable). |
| POST   | `/owner/join-requests/:id/activate` | owner              | **Assign terms**: `{ room_number?, rent_amount, due_day, deposit_amount? }` — flips tenant to `active`, creates deposit due |
| POST   | `/owner/join-requests/:id/reject`   | owner              | Only while join `status=pending` (never finished form). `{ ok: true }` |
| GET    | `/owner/tenants/:id/id-photo`       | owner              | Raw image bytes. Never in list JSON (`has_id_photo` flag only). |

Tenant profile fields: `permanent_address`, `current_address`, `parent_name`, `emergency_phone`, `joined_on` (server date of submit). Personal phone is the login phone.

Walk-in `POST /owner/tenants` remains for phone-less / cash-only people. Do not use it as the default onboarding path.

Events: `JoinRequested`, `JoinApproved`, `JoinRejected`, `TenantCreated` (pending_allocation), `ConsentGiven` (id photo), `DepositTermsAccepted` on assign-terms.

## Owner (role=owner)

| Method | Path                                 | Notes                                                                                        |
| ------ | ------------------------------------ | -------------------------------------------------------------------------------------------- |
| GET    | `/owner/properties`                  | `{ properties: [...] }` — UPI VPA **never** returned                                         |
| GET    | `/owner/tenants`                     | `{ tenants: [...] }` — includes profile fields + `has_id_photo`                              |
| POST   | `/owner/tenants`                     | `{ name, phone?, room_number?, rent_amount, due_day, notice_period_days?, deposit_amount? }` |
| PATCH  | `/owner/tenants/:id`                 | Partial update                                                                               |
| GET    | `/owner/tenants/:id/id-photo`        | Raw ID photo bytes                                                                           |
| POST   | `/owner/tenants/:id/notice`          | Optional `{ notice_given_at }`                                                               |
| POST   | `/owner/tenants/:id/vacate`          |                                                                                              |
| POST   | `/owner/tenants/:id/attach-phone`    | `{ phone }`                                                                                  |
| POST   | `/owner/tenants/:id/prorate`         | `{ vacate_date }`                                                                            |
| POST   | `/owner/tenants/:id/deposit/settle`  | `{ refunded_amount_paise, reason? }`                                                         |
| GET    | `/owner/dues`                        | Query: `tenant_id`, `kind`, `status` → `{ dues: [...] }`                                     |
| POST   | `/owner/dues/:id/waive`              | Returns updated due                                                                          |
| POST   | `/owner/dues/:id/match`              | `{ amount, upi_txn_id }`                                                                     |
| POST   | `/owner/dues/:id/mark-cash-paid`     | `{ amount, note? }` — all-or-nothing                                                         |
| GET    | `/owner/dues/:id/qr`                 | PNG image                                                                                    |
| GET    | `/owner/dues/:id/pay`                | JSON pay payload (see below)                                                                 |
| POST   | `/owner/dues/:id/token`              | `{ path, url, wa_me? }`                                                                      |
| GET    | `/owner/payment-reports`             | Query `status` → `{ payment_reports }`                                                       |
| POST   | `/owner/payment-reports/:id/confirm` | Owner confirm → existing ManualMatch                                                         |
| POST   | `/owner/payment-reports/:id/reject`  | Optional `{ note }`                                                                          |
| GET    | `/owner/payments`                    | Query: `matched_by` → `{ payments: [...] }`                                                  |
| POST   | `/owner/statements/import`           | multipart `file` (CSV)                                                                       |
| GET    | `/owner/reconciliation`              | Query: `period=YYYY-MM` → summary object                                                     |
| GET    | `/owner/events`                      | Query filters → `{ events: [...] }`                                                          |

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

## Tenant (role=tenant, active)

| Method | Path                       | Notes                                                                                                                                              |
| ------ | -------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------- |
| GET    | `/tenant/me`               | Tenant object + `active_dues[]`                                                                                                                    |
| GET    | `/tenant/dues`             | `{ dues: [...] }`                                                                                                                                  |
| GET    | `/tenant/dues/:id/qr`      | PNG (own due only)                                                                                                                                 |
| GET    | `/tenant/dues/:id/pay`     | JSON pay payload (see below)                                                                                                                       |
| POST   | `/tenant/dues/:id/reports` | `{ upi_txn_id, amount?, note? }` or multipart (`image` ≤2MB)                                                                                       |
| GET    | `/tenant/payments`         | `{ payments: [...] }`                                                                                                                              |
| POST   | `/tenant/push/subscribe`   | `{ endpoint, keys: { p256dh, auth } }`                                                                                                             |
| POST   | `/tenant/aadhaar`          | `{ consent: true, qr_payload?, uid_last4?, confirm? }` — Secure QR verify; last-4 only. Without `confirm`, returns decoded fields for user review. |

## Pay JSON

`GET /{owner|tenant}/dues/:id/pay` (VPA is **only** on this payload, never on `GET /owner/properties`):

```json
{
  "mode": "manual",
  "vpa": "owner@upi",
  "upi_link": "upi://pay?pa=...",
  "note": "PG-XXXXXX",
  "due_code": "XXXXXX",
  "amount_paise": 1500000,
  "qr_png_url": "/tenant/dues/<id>/qr",
  "payable": true,
  "payment_session_id": null
}
```

`payable` is false when the due is `paid` or `waived` — hide Pay / save / copy. `mode=cashfree` only when `properties.payment_mode=cashfree` **and** Cashfree keys are configured. While `payment_mode=manual` (default), no Cashfree orders are created even if keys exist. Owner absorbs TDR; tenant `order_amount` equals remaining due; checkout is **UPI-only**; do not enable surcharge/convenience-fee.

When `mode=cashfree`, PNG QR routes return **409** (personal VPA QR is replaced, not shown beside checkout). Use `/pay` JSON `payment_session_id` or the magic-link Cashfree button.

`GET /p/:token` HTML: manual mode = Save QR / Copy UPI ID / Copy `PG-XXXXXX` / Open UPI. Cashfree mode = Pay with UPI checkout (no personal VPA). Hide when paid/waived.

CSV import, owner `match`, and `mark-cash-paid` remain fallbacks after a Cashfree flip.

## Cashfree webhook and poll (dormant)

| Method | Path                 | Notes                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                                 |
| ------ | -------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| POST   | `/webhooks/cashfree` | Raw body HMAC (`x-webhook-signature` = Base64(HMAC-SHA256(timestamp+rawBody, webhook secret)), `x-webhook-timestamp`). `CASHFREE_WEBHOOK_SECRET` defaults to `CASHFREE_SECRET_KEY`. Production with Cashfree enabled fails closed if the secret is still empty. Settles `PAYMENT_SUCCESS_WEBHOOK` only when `payment_amount` matches `payment_intents.amount_paise`, via `settleMatched` with `matched_by=cashfree`. Idempotent on `cf_payment_id` and `payments.upi_txn_id`. `MarkPaid` only after settle or duplicate UTR. Unknown order → 200; DB errors → 500. Failed/dropped types return 200 and do not change the due. Do not call Cashfree from this handler. |

Missed-webhook poll: `go run ./cmd/cashfree-poll/` — intents `created` older than 10 minutes whose due is still `pending`/`partial`. No-op without `CASHFREE_APP_ID` / `CASHFREE_SECRET_KEY`.

Flip live only after KYC: production keys, then `UPDATE properties SET payment_mode='cashfree'`. Sandbox keys + `CASHFREE_ENV=sandbox` until then.

D+1/D+7 reminders for `payment_mode=cashfree` require a payment-intent for that due with `updated_at` in the last 24h (poll touches on every fetch), or no intents yet (tenant never started checkout). Manual mode still requires a CSV import in the last 24h.

## Payment reports (UTR proof)

Unique `upi_txn_id` on `payment_reports` and `payments`. Duplicate UTR → 409. Already paid/waived due → 409 `"already recorded"`. Owner confirm reuses ManualMatch. Cash is **not** this path — owner `mark-cash-paid` only. OCR sidecar is v1.1 (not in this API).

Events: `PaymentReportSubmitted`, `PaymentReportRejected`.

## Public

| Method | Path                       | Notes                                                                                           |
| ------ | -------------------------- | ----------------------------------------------------------------------------------------------- |
| GET    | `/healthz`                 | `{ "status": "ok" }`                                                                            |
| GET    | `/push/vapid-public-key`   | `{ "public_key": "..." }`                                                                       |
| GET    | `/app/`                    | Dev-only in-repo PWA. Production (`APP_ENV=production` + `FRONTEND_URL`) redirects to pg-react. |
| GET    | `/p/:token`                | HTML payment page (save QR, copy VPA/note, open UPI)                                            |
| POST   | `/p/:token/push/subscribe` | Push from payment page                                                                          |

## Postman

Import `postman/pg-go.postman_collection.json` and `postman/pg-go.postman_environment.json`.

1. Sign in with Firebase (Phone OTP or Google + linked phone) and copy the ID token
2. Set `id_token` (and `invite_code` for a new tenant phone)
3. Run **Firebase Exchange** — `token` and `tenant_id` auto-populate
4. Owners skip invite; pending tenants call Join endpoints until activate, then exchange again so the JWT includes `tenant_id`

## CORS

Set in `.env`:

```env
CORS_ALLOWED_ORIGINS=http://localhost:5173,https://your-app.pages.dev
```

Required for pg-react dev (`localhost:5173`).
