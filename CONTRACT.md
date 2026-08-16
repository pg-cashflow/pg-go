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

Login is Firebase Phone OTP or Google (Google must have a linked phone). There is **no Owner vs Tenant role picker**. Invite code is how a new phone becomes a pending tenant. Owner phones stay seeded.

| Method | Path             | Body                             | Response                                                             |
| ------ | ---------------- | -------------------------------- | -------------------------------------------------------------------- |
| POST   | `/auth/firebase` | `{ "id_token", "invite_code"? }` | `{ "token", "user": { id, phone, role, tenant_id?, property_id? } }` |

| Status | When                                                                               |
| ------ | ---------------------------------------------------------------------------------- |
| 401    | Invalid or phone-less Firebase token                                               |
| 403    | Tenant vacated                                                                     |
| 404    | Unknown phone without a valid invite — `"Get the PG invite code from your owner."` |
| 503    | Firebase Admin not configured                                                      |

Unknown phone + **valid invite** creates `users.role=tenant` with `tenant_id` null and a `join_requests` row. Pending tenants calling `/tenant/*` get 403 `"waiting for owner to assign room and rent"`.

Rent-reminder SMS still uses `SMS_*` / `internal/sms`. Login OTP is not sent by this API.

## Join (tenant-first)

| Method | Path                                | Auth               | Notes                                                                                                                                             |
| ------ | ----------------------------------- | ------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------- |
| GET    | `/join/invite/:code`                | public             | `{ property_id, property_name, owner_name }` — never VPA                                                                                          |
| GET    | `/join/me`                          | pending tenant JWT | `{ join, message }` waiting screen                                                                                                                |
| POST   | `/join`                             | pending tenant JWT | `{ name, qr_payload?, uid_last4?, consent, confirm? }` — tenant never sends rent. QR without `confirm` returns `{ aadhaar, needs_confirm: true }` |
| GET    | `/owner/invite`                     | owner              | `{ invite_code, payment_mode }`                                                                                                                   |
| POST   | `/owner/invite/rotate`              | owner              | `{ invite_code }`                                                                                                                                 |
| GET    | `/owner/join-requests`              | owner              | Query `status` → `{ join_requests }`                                                                                                              |
| POST   | `/owner/join-requests/:id/activate` | owner              | `{ room_number?, rent_amount, due_day, deposit_amount? }` — owner sets money terms; calls existing CreateTenant                                   |
| POST   | `/owner/join-requests/:id/reject`   | owner              | `{ ok: true }`                                                                                                                                    |

Walk-in `POST /owner/tenants` remains for phone-less / cash-only people. Do not use it as the default onboarding path.

Events: `JoinRequested`, `JoinApproved`, `JoinRejected`.

## Owner (role=owner)

| Method | Path                                 | Notes                                                                                        |
| ------ | ------------------------------------ | -------------------------------------------------------------------------------------------- |
| GET    | `/owner/properties`                  | `{ properties: [...] }` — UPI VPA **never** returned                                         |
| GET    | `/owner/tenants`                     | `{ tenants: [...] }`                                                                         |
| POST   | `/owner/tenants`                     | `{ name, phone?, room_number?, rent_amount, due_day, notice_period_days?, deposit_amount? }` |
| PATCH  | `/owner/tenants/:id`                 | Partial update                                                                               |
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

| Method | Path                 | Notes                                                                                                                                                                                                                                                                                                                                                                                                                  |
| ------ | -------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| POST   | `/webhooks/cashfree` | Raw body HMAC (`x-webhook-signature` = Base64(HMAC-SHA256(timestamp+rawBody, webhook secret)), `x-webhook-timestamp`). `CASHFREE_WEBHOOK_SECRET` defaults to `CASHFREE_SECRET_KEY`. Production with Cashfree enabled fails closed if the secret is still empty. Settles `PAYMENT_SUCCESS_WEBHOOK` only when `payment_amount` matches `payment_intents.amount_paise`, via `settleMatched` with `matched_by=cashfree`. Idempotent on `cf_payment_id` and `payments.upi_txn_id`. `MarkPaid` only after settle or duplicate UTR. Unknown order → 200; DB errors → 500. Failed/dropped types return 200 and do not change the due. Do not call Cashfree from this handler. |

Missed-webhook poll: `go run ./cmd/cashfree-poll/` — intents `created` older than 10 minutes whose due is still `pending`/`partial`. No-op without `CASHFREE_APP_ID` / `CASHFREE_SECRET_KEY`.

Flip live only after KYC: production keys, then `UPDATE properties SET payment_mode='cashfree'`. Sandbox keys + `CASHFREE_ENV=sandbox` until then.

D+1/D+7 reminders for `payment_mode=cashfree` require a payment-intent for that due with `updated_at` in the last 24h (poll touches on every fetch), or no intents yet (tenant never started checkout). Manual mode still requires a CSV import in the last 24h.

## Payment reports (UTR proof)

Unique `upi_txn_id` on `payment_reports` and `payments`. Duplicate UTR → 409. Already paid/waived due → 409 `"already recorded"`. Owner confirm reuses ManualMatch. Cash is **not** this path — owner `mark-cash-paid` only. OCR sidecar is v1.1 (not in this API).

Events: `PaymentReportSubmitted`, `PaymentReportRejected`.

## Public

| Method | Path                       | Notes                                                |
| ------ | -------------------------- | ---------------------------------------------------- |
| GET    | `/healthz`                 | `{ "status": "ok" }`                                 |
| GET    | `/push/vapid-public-key`   | `{ "public_key": "..." }`                            |
| GET    | `/app/`                    | In-repo PWA (invite → wait / owner queue / pay)      |
| GET    | `/p/:token`                | HTML payment page (save QR, copy VPA/note, open UPI) |
| POST   | `/p/:token/push/subscribe` | Push from payment page                               |

## Postman

Import `postman/pg-go.postman_collection.json` and `postman/pg-go.postman_environment.json`.

1. Set `phone` to a seeded owner/tenant phone
2. Run **OTP Request**, then **OTP Verify** (enter OTP from SMS/logs)
3. `token`, `tenant_id` auto-populate for subsequent requests

## CORS

Set in `.env`:

```env
CORS_ALLOWED_ORIGINS=http://localhost:5173,https://your-app.pages.dev
```

Required for pg-react dev (`localhost:5173`).
