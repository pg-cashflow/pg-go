# pg-go

Automated rent-due tracking and UPI payment matching for a PG/hostel — per-tenant anniversary billing, per-Due QR collection, and reminder/escalation automation.

Phase 1 is a standalone Go API + Neon Postgres service (React PWA is Phase 2).

**Documentation:** Start at [docs/README.md](docs/README.md) for architecture ([ARCHITECTURE.md](docs/ARCHITECTURE.md)), codebase map ([CODEBASE.md](docs/CODEBASE.md)), and debugging ([DEBUGGING.md](docs/DEBUGGING.md)). Prose follows ASD-STE100; use the Karpathy order in DEBUGGING when investigating failures.

## Quick start

```bash
cp .env.example .env
# set DATABASE_URL, JWT_SECRET, OTP_HMAC_SECRET, MAGIC_LINK_HMAC_SECRET,
# FIREBASE_PROJECT_ID, GOOGLE_APPLICATION_CREDENTIALS (Admin service-account JSON).

go run ./cmd/migrate/
go run ./cmd/server/
```

Or use `make migrate` / `make server` (see [Makefile](Makefile)).

## Commands

| Cmd | Purpose |
| --- | --- |
| `cmd/server` | HTTP API and background workers (notification outbox, ledger outbox) |
| `cmd/migrate` | Apply `migrations/*.sql` |
| `cmd/billing-cycle` | Daily anniversary rent dues (00:05 IST) |
| `cmd/reminder` | Daily reminders D-3/D-0/D+1/D+7 (09:00 IST) |
| `cmd/cashfree-poll` | Missed Cashfree webhook settle (no-op if keys unset) |
| `cmd/daily-rollup` | Daily rollup job |
| `cmd/digilocker-reconcile` | DigiLocker reconciliation sweep |
| `cmd/financial-summary` | `--cadence=monthly\|yearly` collections and finance digest email |
| `cmd/gamification-cycle` | Gamification periodic job |
| `cmd/kpi-snapshot` | Daily KPI and ROI snapshot |
| `cmd/kyc-expiry` | Hourly KYC expiry sweep |
| `cmd/sandbox-smoke` | Cashfree sandbox smoke harness |

## Locked Phase 1 decisions

- **D2** Cash is all-or-nothing against remaining `due.amount` (no cash partial).
- **D3** Phone-less tenants are rare; no `cash_notes` column; use `mark-cash-paid` + `attach-phone`.

## Tests

```bash
go test ./...
```

With database: `make test-db` or `REQUIRE_DB=1 go test -count=1 ./...`. See [docs/DEBUGGING.md](docs/DEBUGGING.md).

## Frontend integration (pg-react)

Phase 2 PWA lives in a separate repo. To connect locally:

1. Set `CORS_ALLOWED_ORIGINS=http://localhost:5173` in `.env`
2. Run `go run ./cmd/server/`
3. In pg-react, set `VITE_API_BASE_URL=http://localhost:8080/api` and `VITE_CASHFREE_ENV` to match `CASHFREE_ENV`

The in-repo `/app/` PWA is a frozen fallback when `APP_ENV` is not `production` or `FRONTEND_URL` is empty. Production should set `FRONTEND_URL` to the Cloudflare Pages origin. Embedded pg-react assets live in `internal/web/dist/` (SPA catch-all).

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
