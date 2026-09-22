# pg-go

Automated rent-due tracking and UPI payment matching for a PG/hostel — per-tenant anniversary billing, per-Due QR collection, and reminder/escalation automation.

Phase 1 is a standalone Go API + Neon Postgres service (React PWA is Phase 2).

## Quick start

```bash
cp .env.example .env
# set DATABASE_URL, JWT_SECRET, OTP_HMAC_SECRET, MAGIC_LINK_HMAC_SECRET,
# FIREBASE_PROJECT_ID, GOOGLE_APPLICATION_CREDENTIALS (Admin service-account JSON).

go run ./cmd/migrate/
go run ./cmd/server/
```

## Commands

| Cmd                     | Purpose                                              |
| ----------------------- | ---------------------------------------------------- |
| `cmd/server`            | HTTP API                                             |
| `cmd/migrate`           | Apply `migrations/*.sql`                             |
| `cmd/billing-cycle`     | Daily anniversary rent dues (00:05 IST)              |
| `cmd/reminder`          | Daily reminders D-3/D-0/D+1/D+7 (09:00 IST)          |
| `cmd/cashfree-poll`     | Missed Cashfree webhook settle (no-op if keys unset) |
| `cmd/financial-summary` | `--cadence=monthly\|yearly` collections & finance digest email (extended with OCF/TBE; not replaced) |
| `cmd/kpi-snapshot`      | Daily KPI and ROI snapshot calculation and persist   |
| `cmd/kyc-expiry`        | Hourly KYC expiry sweep (stale pending TTL & lapsed verified PII scrub) |

## Locked Phase 1 decisions

- **D2** Cash is all-or-nothing against remaining `due.amount` (no cash partial).
- **D3** Phone-less tenants are rare; no `cash_notes` column; use `mark-cash-paid` + `attach-phone`.

## Tests

```bash
go test ./...
```

## Frontend integration (pg-react)

Phase 2 PWA lives in a separate repo. To connect locally:

1. Set `CORS_ALLOWED_ORIGINS=http://localhost:5173` in `.env`
2. Run `go run ./cmd/server/`
3. In pg-react, set `VITE_API_BASE_URL=http://localhost:8080/api` and `VITE_CASHFREE_ENV` to match `CASHFREE_ENV`

The in-repo `/app/` PWA is a frozen fallback for local `APP_ENV=development`. Production should set `FRONTEND_URL` to the Cloudflare Pages origin so `/` and `/app/` redirect to pg-react.

Login: Firebase Phone (test numbers on Spark) or Google + linked phone → `POST /auth/firebase` → app JWT.

Place the Firebase Admin service account at `./secrets/firebase-sa.json`. Without it, `POST /auth/firebase` returns 503 in development.

### Production Firebase (after test-number login works)

1. Upgrade Firebase project `pg-cashflow-prod` to Blaze
2. Auth → Settings → Authorized domains: production host only (no `https://`, no port)
3. Phone provider: allow India SMS region
4. Set billing alerts on the linked Google Cloud billing account
5. Keep test numbers for local/dev; try one real +91 last

Firebase Hosting is not required. Reminder SMS stays on `SMS_*` (Android gateway).

See [CONTRACT.md](CONTRACT.md) (Rev 8) for the full HTTP API (including finance, ROI, intelligence, manager, gamification, search, notifications) and [postman/](postman/) for a Postman collection. See [docs/finance_roi.md](docs/finance_roi.md) and [docs/adr/](docs/adr/) for financial architecture and ADRs.
