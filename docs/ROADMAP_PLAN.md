# Production Launch Roadmap & Engineering Plan (pg-go)

## 1. Operating Decisions & Architecture Agreement

- **CI / GitHub Actions:** Held on hold indefinitely. No workflow dispatch or automated push triggers will be added or modified for now. All verification, tests, and builds run directly on the developer / server environment.
- **Database (Postgres):** Sized and hosted on the self-managed **PG-LIVE VM** (plan to bump RAM to 4 GB).
  - Isolated on dedicated disk/VM.
  - Backups: Nightly `pg_dump` + WAL archiving to Cloudflare R2 for point-in-time recovery (PITR).
  - Cron/Job execution: Run as native `systemd` timers on the server host rather than external GitHub runners or serverless invokes.
- **Search (pgvector vs Lexical):** Keep the `search_documents` table structure from `011_search_semantic.sql` for lexical searching, but make `pgvector` / `embedding vector(384)` / HNSW index strictly optional via dynamic extension check (`pg_available_extensions`). Pure lexical search relies on `pg_trgm` and `ILIKE` queries (from `010_search_lexical.sql`). Update `Indexer` so document search functions with title/body without requiring an active embedder.
- **Messaging & Notifications:**
  - **Reminders (D-3, D-0, D+1, D+7):** Android SMS Relay (Device A primary + Device B fallback SIM).
  - **App Notifications:** Web Push via VAPID (`internal/push/service.go`) for installed PWA.
  - **FCM (Firebase Cloud Messaging):** Deferred until a native Android/iOS shell/wrapper is introduced.
  - **Reliability:** Implement a catch-up window for missed reminder runs using the deduplication log table.
- **Owner Dashboard & Per-Property Feature Toggles:**
  - Toggles stored in Postgres per-property (with audit logging & step-up OTP for financial toggles).
  - Controlled features: Payout auto-dispatch, reminder offsets/channels, gamification, calendar feed, finance modules.
  - Non-negotiable security core: Webhook HMAC verification, double-entry ledger balance, payout maker-checker, tenant IDOR checks.
- **Bulk Mark-Paid Flow:**
  - **Preview:** Computes count, total paise, filters out already-paid or gateway-settled dues.
  - **Confirm:** Requires Step-up OTP with re-entered confirmation total to mitigate physical cash reconciliation and revenue inflation risk.
  - **Execute:** Atomic single-transaction write taking deterministic row locks (`tenants` -> `streaks` -> `dues` -> `payments`), linked via unique `batch_ref`.
- **Calendar Feed:**
  - Read-only `.ics` feed authenticated via property-scoped HMAC secret token.
  - Anonymized (shows aggregated due count & totals, no tenant personal data). Built after core financial flows.

---

## 2. Phased Delivery Slices

### Slice 0: Baseline & Migration Hygiene
1. Run local test suite (`go clean -testcache && go test -count=1 ./...`) and record actual baseline pass/fail status without assumptions.
2. Refactor `migrations/011_search_semantic.sql` to make the `vector` extension and embedding column optional conditionally on `pg_available_extensions`.
3. Update `Indexer` to allow populating `search_documents` without an active embedder so lexical search works.
4. Keep all GitHub Actions workflows frozen/on-hold.

### Slice 1: Core Money Rail Verification (Sandbox)
1. End-to-end sandbox validation: Order creation -> Cashfree UPI/Checkout -> Webhook receipt & HMAC validation -> Double-entry journal posting.
2. Settlement reconciliation & refund handling.
3. Verification of fee & tax handling against Cashfree settlement specs.

### Slice 2: Scheduler, Settings & Reliability
1. Generate `systemd` service and timer unit definitions for all background jobs:
   - `billing-cycle`
   - `cashfree-poll`
   - `digilocker-reconcile`
   - `financial-summary`
   - `gamification-cycle`
   - `kpi-snapshot`
   - `kyc-expiry`
   - `reminder`
   - `search-reindex`
2. Add per-property settings table migration (payout auto-dispatch, reminder offsets, active modules).
3. Fix missed-reminders: Introduce a grace/catch-up window in `internal/jobs/reminder.go`.

### Slice 3: Dashboard Backend Extensions
1. `GET /owner/occupancy` aggregate endpoint.
2. Bulk mark-cash-paid flow:
   - `POST /owner/dues/bulk-mark-paid/preview`
   - `POST /owner/dues/bulk-mark-paid/confirm` (atomic batch execution with step-up verification).
3. Recurring expenses (utilities, vendor retainer) scheduling hook in billing cycle.
4. Anonymized `.ics` calendar subscription endpoint (`GET /owner/calendar.ics?token=...`).

### Slice 4: Payouts & Statement Fallbacks
1. Cashfree Transfers V2 sandbox flow: Payee creation -> Batch creation -> Step-up OTP -> Dispatch -> Status webhook.
2. Bank statement parser fixtures and regression tests for SBI and HDFC CSV exports.
3. Payout auto-dispatch toggle integration.

### Slice 5: Hardening & Pre-launch Audit
1. Run secret scan (`gitleaks`) and vulnerability scan (`govulncheck`).
2. Verification of IDOR boundaries across all `/owner/*` and `/tenant/*` routes.
3. Connection pool tuning & load ceiling assessment for 4 GB PG-LIVE host.

### Slice 6: PWA Contract Freeze
1. Review and lock API contract with `pg-react`.
2. Verify iOS Safari Web Push gesture & permission requirements.
