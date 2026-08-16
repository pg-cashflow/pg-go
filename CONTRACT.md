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

Login is Firebase Phone OTP or Google (Google must have a linked phone). The PWA sends the Firebase ID token; this API verifies it, links `users.firebase_uid`, and issues an app JWT. Unknown phones (not an active tenant or property owner) return 404.

| Method | Path             | Body             | Response                                                             |
| ------ | ---------------- | ---------------- | -------------------------------------------------------------------- |
| POST   | `/auth/firebase` | `{ "id_token" }` | `{ "token", "user": { id, phone, role, tenant_id?, property_id? } }` |

| Status | When |
| ------ | ---- |
| 401 | Invalid or phone-less Firebase token |
| 403 | Tenant vacated |
| 404 | No local account for the verified phone |
| 503 | Firebase Admin not configured |

Rent-reminder SMS still uses `SMS_*` / `internal/sms`. Login OTP is not sent by this API.

## Owner (role=owner)

| Method | Path                                | Notes                                                                                        |
| ------ | ----------------------------------- | -------------------------------------------------------------------------------------------- |
| GET    | `/owner/properties`                 | `{ properties: [...] }` — UPI VPA hidden                                                     |
| GET    | `/owner/tenants`                    | `{ tenants: [...] }`                                                                         |
| POST   | `/owner/tenants`                    | `{ name, phone?, room_number?, rent_amount, due_day, notice_period_days?, deposit_amount? }` |
| PATCH  | `/owner/tenants/:id`                | Partial update                                                                               |
| POST   | `/owner/tenants/:id/notice`         | Optional `{ notice_given_at }`                                                               |
| POST   | `/owner/tenants/:id/vacate`         |                                                                                              |
| POST   | `/owner/tenants/:id/attach-phone`   | `{ phone }`                                                                                  |
| POST   | `/owner/tenants/:id/prorate`        | `{ vacate_date }`                                                                            |
| POST   | `/owner/tenants/:id/deposit/settle` | `{ refunded_amount_paise, reason? }`                                                         |
| GET    | `/owner/dues`                       | Query: `tenant_id`, `kind`, `status` → `{ dues: [...] }`                                     |
| POST   | `/owner/dues/:id/waive`             | Returns updated due                                                                          |
| POST   | `/owner/dues/:id/match`             | `{ amount, upi_txn_id }`                                                                     |
| POST   | `/owner/dues/:id/mark-cash-paid`    | `{ amount, note? }` — all-or-nothing                                                         |
| GET    | `/owner/dues/:id/qr`                | PNG image                                                                                    |
| POST   | `/owner/dues/:id/token`             | `{ path, url, wa_me? }`                                                                      |
| GET    | `/owner/payments`                   | Query: `matched_by` → `{ payments: [...] }`                                                  |
| POST   | `/owner/statements/import`          | multipart `file` (CSV)                                                                       |
| GET    | `/owner/reconciliation`             | Query: `period=YYYY-MM` → summary object                                                     |
| GET    | `/owner/events`                     | Query filters → `{ events: [...] }`                                                          |

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

| Method | Path                     | Notes                                  |
| ------ | ------------------------ | -------------------------------------- |
| GET    | `/tenant/me`             | Tenant object + `active_dues[]`        |
| GET    | `/tenant/dues`           | `{ dues: [...] }`                      |
| GET    | `/tenant/dues/:id/qr`    | PNG (own due only)                     |
| GET    | `/tenant/payments`       | `{ payments: [...] }`                  |
| POST   | `/tenant/push/subscribe` | `{ endpoint, keys: { p256dh, auth } }` |
| POST   | `/tenant/aadhaar`        | `{ consent: true, qr_payload?, ... }`  |

## Public

| Method | Path                       | Notes                               |
| ------ | -------------------------- | ----------------------------------- |
| GET    | `/healthz`                 | `{ "status": "ok" }`                |
| GET    | `/push/vapid-public-key`   | `{ "public_key": "..." }`           |
| GET    | `/p/:token`                | HTML payment page (server-rendered) |
| POST   | `/p/:token/push/subscribe` | Push from payment page              |

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
