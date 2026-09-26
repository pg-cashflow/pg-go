# PG Cashflow Backend: Consolidated Production-Readiness Plan & Audit Manifest

> **Status**: Verified Production-Ready for Phase 1 Deployment  
> **Target Scale**: 1–10 Properties · 50–500 Tenants · 5–20 Staff  
> **Repository**: `github.com/pg-cashflow/pg-go`  
> **Active Branches**: `origin/div_dev` & `origin/develop` (in sync at commit `ccea9fc`)

---

## 1. Executive Architecture Summary

PG Cashflow backend is built as a **hardened modular monolith** with single-tenant Postgres persistence and cron-driven background workers:

1. **Topology**: Single Go module (`github.com/pg-cashflow/pg-go`), single Postgres database, and 11 distinct binary entrypoints in `cmd/`:
   - `server`: Core HTTP REST API serving `/api/*` and embedding the static React PWA SPA via `embed.FS` from `internal/web/dist/`.
   - 10 single-purpose cron workers: `billing-cycle`, `reminder`, `cashfree-poll`, `digilocker-reconcile`, `kyc-expiry`, `financial-summary`, `kpi-snapshot`, `gamification-cycle`, `search-reindex`, and `migrate`.
2. **Persistence & Concurrency**:
   - Zero microservice network hops or distributed two-phase commits.
   - All state mutations (rent generation, collections, maker-checker approvals, departures) execute inside ACID PostgreSQL transactions using row-level locking (`SELECT ... FOR UPDATE`).
3. **Deployment Strategy**:
   - Single static binary build with `-trimpath -ldflags="-s -w"`.
   - Automated deployment triggered via `workflow_dispatch` on dedicated self-hosted runner `[self-hosted, pg-app-server]`.

---

## 2. Invariant & Security Enforcement Matrix

| Track / Area | Target Risk | Remediation & Cryptographic Guarantee | Verification Status |
|---|---|---|---|
| **Track 0: Financial Math & IDOR** | Float precision loss, balance leaks, IDOR | Pure integer-paise math throughout (`AmountPaise`, `int64`). `MirrorDepartureSettlement` balances double-entry ledger $\sum\text{Debits} == \sum\text{Credits}$. 4x IDOR guards added on payout routes. | **Verified** (`34ec648`) |
| **Track A: Identity & KYC** | Aadhaar storage liability, DPDP violation | RSA-2048 UIDAI QR signature verification is fail-closed. DPDP consent gating with atomic PII wipe on revocation cascade. Zero raw 12-digit Aadhaar storage. 30s distributed mutex lease with 60s cooldown. | **Verified** (ADR-004, `internal/kyc`) |
| **Track B: Payment Collections** | Webhook replay, double-credit, tampering | Timing-safe `hmac.Equal`, atomic first-seen dedup in `webhook_events` table under due row-lock (`FOR UPDATE`). Re-asserts `amount == intent.AmountPaise`. Loser payments converted to tracked tenant credit. | **Verified** (`af4ed81`, `cashfree_poll.go`) |
| **Track C / C.2: Payout Controls** | Unauthorized fund disbursement, tampering | Draft-first lifecycle (`domain.BatchDraft`). Affirmative verification of item count and total paise. Multi-owner dual-control maker-checker (`UserID != CreatedBy`). OWASP CSV formula injection prefix sanitization (`'`, `+`, `-`, `=`, `@`). | **Verified** (`2bc013b`, `track_c2`) |
| **Track C.3: Step-Up Reauth** | Account takeover on solo-owner approval | Cryptographic step-up reauthentication. Dedicated trigger `POST .../approve/request-otp` with purpose-parameterized SMS. Unexpired, single-use, attempt-locked OTP records (preventing replay). Firebase token `auth_time` checked $\le 5$ minutes. | **Verified** (`d313d21`, `internal/auth/step_up.go`) |
| **Track D: Cross-Cutting IDOR** | Tenant horizontal privilege escalation | Tenant ID claims binding across all gamification, menu voting, and finance endpoints. Cross-tenant tampering strictly rejected. | **Verified** (`track_d`) |
| **Track E: Departure Outbox** | Silently unmirrored ledger records on crash | Dedicated `ledger_outbox_events` table enqueued within `SettleDepartureUnderLock` before `tx.Commit`. Asynchronous `LedgerOutboxWorker` with row-level `SKIP LOCKED` and dead-letter escalation. | **Verified** (`track_e`) |
| **Ticket 5: Background Packages** | Dedup race, send failures | Atomic `TryLog` slot acquisition before notification dispatch with `DeleteLog` rollback on delivery error in `reminder.go`. Table-driven tests for SMS dual-gateway failover and WebPush lifecycle. | **Verified** (`35ee7bf`, `74e95af`) |
| **Ticket 6: Automated Security** | Leaked credentials, vulnerable dependencies | Gitleaks scan 100% clean across all 19 commits. `govulncheck` verified 0 reachable symbol vulnerabilities across 42 packages. Transitive modules bumped (`grpc@v1.83.2`, `x/crypto@v0.56.0`). | **Verified** (`8e55f48`) |
| **Ticket 7: Capacity & Scaling** | 1,000+ user bursts, pool starvation, PgBouncer leaks | Configurable `DATABASE_MAX_CONNS` (default 25) with proportional `MinConns`. Streamlined `pg_advisory_xact_lock` inside migration transactions. `http.Server` production timeouts (30s read, 60s write, 120s idle). Bounded rate limiter (10,000 cap, 10m TTL sweep). | **Verified** (`871e4b8`, `ccea9fc`) |

---

## 3. Capacity & Runtime Profiles (1,000+ Concurrent Scale)

### A. Database Connection Pool & PgBouncer Strategy
* **Connection Pool Ceiling**: Configured via `DATABASE_MAX_CONNS` (defaults to 25). `MinConns` scales automatically (`max(2, maxConns/5)`).
* **PgBouncer Multiplexing Safety**:
  * In `internal/postgres/db.go`, `Migrate()` uses transaction-scoped advisory locks (`SELECT pg_advisory_xact_lock($1)`) strictly inside individual migration transactions.
  * In transaction-mode pooling (e.g., Neon `-pooler` endpoints), all statements in a transaction block are pinned to the same physical connection.
  * Session-level lock leakage is completely eliminated.
  * `cmd/migrate` checks for direct unpooled endpoints (`DATABASE_URL_UNPOOLED` or `DIRECT_URL`) before falling back to `cfg.DatabaseURL`.

### B. HTTP Server Timeout Envelope
In `cmd/server/main.go`, `http.Server` enforces strict timeouts:
* `ReadHeaderTimeout: 10s` (Slowloris mitigation)
* `ReadTimeout: 30s` (Large upload protection)
* `WriteTimeout: 60s` (Generous enough for slow mobile 2G/3G CSV batch exports, while terminating zombie connections)
* `IdleTimeout: 120s` (Keep-alive connection pool recycling)

### C. In-Memory Rate Limiting
In `internal/api/ratelimit.go`:
* **Token Bucket**: Sustained rate and burst configured per endpoint (e.g. OTP and login endpoints).
* **Hard Memory Cap**: Hard ceiling at 10,000 unique IP entries (~1MB memory footprint).
* **TTL Sweep**: Inactive entries (>10 minutes) are purged upon capacity saturation.
* **Oldest-Entry Eviction**: If saturated by a sudden distributed burst of distinct IPs within the TTL window, the oldest active entry is evicted to admit the new IP without unbounded allocation.

### D. Delivery Infrastructure: SMS vs. Push Notifications
* **Primary Backbone (SMS)**:
  * [AndroidGateway](file:///c:/Users/divak/Downloads/pg-go/internal/sms/android.go) uses primary relay with automatic failover to secondary relay.
  * If both legs fail, it immediately fires an email alert to the property owner/admin via [MailAlert](file:///c:/Users/divak/Downloads/pg-go/internal/sms/alert.go).
* **Supplementary Push (Web Push RFC 8292 / VAPID)**:
  * **Android**: 100% native Web Push support across modern browsers and installable PWAs.
  * **iOS (16.4+)**: Web Push is supported exclusively when the tenant uses Safari's "Add to Home Screen" to install the PWA.
  * **Hygiene**: Returned HTTP 404/410 (Gone) automatically triggers `DeleteByEndpoint`, preventing dead endpoint database bloat.
  * **Scope**: Push is strictly supplementary; critical dues and step-up codes rely on SMS and in-app notification feeds.

---

## 4. Branching Model & Release Discipline

* **`div_dev`**: Primary active development and feature branch.
* **`develop`**: Stable integration branch. Pushed in lockstep with `div_dev` once all tests pass 100%.
* **`main`**: Production deployment branch.
  * Triggered exclusively via `workflow_dispatch` in `.github/workflows/deploy.yml` until the self-hosted runner (`pg-app-server`) is online.

---

## 5. Pre-Launch Production Checklist

Before toggling public traffic on the production server:

- [ ] **Provision Self-Hosted Runner**: Register Ubuntu Linux host as GitHub Actions runner labeled `[self-hosted, pg-app-server]`.
- [ ] **Configure Environment Secrets**:
  - `DATABASE_URL`: Pooled connection string for application runtime.
  - `DATABASE_URL_UNPOOLED` / `DIRECT_URL`: Direct unpooled connection string for `cmd/migrate`.
  - `DATABASE_MAX_CONNS`: Sized according to Neon compute tier (e.g. 25–50).
  - `JWT_SECRET`, `OTP_HMAC_SECRET`, `MAGIC_LINK_HMAC_SECRET`: 32+ character high-entropy cryptographically random strings.
  - `CASHFREE_PG_APP_ID`, `CASHFREE_PG_SECRET_KEY`, `CASHFREE_PG_WEBHOOK_SECRET`: Production Cashfree credentials.
  - `CASHFREE_ENV`: Set to `production`.
  - `SMS_PRIMARY_URL`, `SMS_PRIMARY_API_KEY`, `SMS_FALLBACK_URL`, `SMS_FALLBACK_API_KEY`: Android SMS gateway credentials.
  - `VAPID_PUBLIC_KEY`, `VAPID_PRIVATE_KEY`, `VAPID_SUBJECT`: Web Push VAPID credentials.
- [ ] **Run Live Synthetic Load Test**:
  - Execute a baseline k6/vegeta load test targeting 200 req/sec across dues viewing, payment intent generation, and in-app notification feeds to establish baseline latency and connection metrics.
- [ ] **Quarterly UIDAI Key Rotation Runbook**:
  - Maintain the documented quarterly rotation schedule for `AADHAAR_QR_PUBLIC_KEY_PEM`.
