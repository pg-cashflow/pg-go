# Ticket 16: Track Q — Final Staging Cutover & End-to-End Deployment Verification

- **Type**: `wayfinder:task`
- **Status**: Resolved
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Prerequisite**: Track P (End-of-Day Multi-Way Settlement Balancer) resolved.

---

## 1. Objective

Complete final pre-deployment audit, binary compilation verification, database migration integrity check, and frontend-backend contract tie-out across `pg-go` and `pg-react`.

## 2. Key Verifications

1. **Database Migrations Integrity (001–044)**:
   - All 44 SQL migrations in `migrations/` verified idempotent (`CREATE TABLE IF NOT EXISTS`, `CREATE INDEX IF NOT EXISTS`).
   - Clean linear dependency chain from `001_initial.sql` up to `044_ledger_period_lock_and_reporting.sql`.
   - `cmd/migrate` builds and executes cleanly with PgBouncer-compatible advisory locking (verified via `go run cmd/migrate/main.go`).

2. **Binary Buildability**:
   - All 12 executable binaries in `cmd/...` compile with 0 errors:
     - `cmd/server` (Core HTTP API with embedded SPA & static assets)
     - `cmd/billing-cycle`
     - `cmd/cashfree-poll`
     - `cmd/digilocker-reconcile`
     - `cmd/financial-summary`
     - `cmd/gamification-cycle`
     - `cmd/kpi-snapshot`
     - `cmd/kyc-expiry`
     - `cmd/migrate`
     - `cmd/reminder`
     - `cmd/sandbox-smoke`
     - `cmd/search-reindex`

3. **Backend Test Suite Parity**:
   - 100% green test suite across all 31 internal packages in `pg-go`.
   - Invariant evaluations pass with 0 integer-paise drift:
     - Departure double-entry conservation (10,000 iterations)
     - Bank statement quarantine and allocation (10,000 iterations)
     - Multi-way settlement balancer property evals (10,000 iterations)
     - Imbalance perturbation fail-closed rejection (2,500 iterations)

4. **Frontend PWA Quality & Parity**:
   - `pg-react` vitest test suite: 16/16 tests pass (`npm test`).
   - Linting: 0 errors across 113 files; zero palette drift (`check-palette.mjs` OK), zero typography scale violations (`check-typography.mjs` OK).
   - Production bundle: `tsc -b && vite build` generates optimized client and service worker (`src/sw.ts`) with zero TypeScript errors.

5. **Security & Cryptographic Guarantees**:
   - Gate 1 & 2: `TRUSTED_PROXIES`, `SecurityHeaders`, `MaxBodyBytes(10MB)`, pure integer paise arithmetic.
   - Gate 3: ADR-007 Auth Token Rotation with short-lived 15m JWTs and 30-day HttpOnly cookie with family replay detection.
   - Gate 4: Cashfree Sandbox Live Smoke Test passed.
   - Idempotency: `Idempotency-Key` enforced per user intent.
