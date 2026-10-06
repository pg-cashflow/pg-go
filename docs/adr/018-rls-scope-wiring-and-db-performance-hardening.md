# ADR-018: RLS Scope Wiring, Role Separation, and Database Performance Hardening

**Status:** Proposed (parts 1, 4, 5 below are already implemented in this change; parts 2 and 3 need sign-off)
**Date:** 2026-10-06
**Deciders:** Engineering Lead (owner of pg-go)
**Builds on:** [ADR-015](015-ledger-integrity-db-controls.md), [ADR-016](016-ledger-period-lock-statements-completeness.md), [ADR-017](017-postgres-cluster-replicas-pgbouncer-sharding.md)

---

## Context

A review of the schema, repositories, jobs and deploy config found one correctness gap and several
cheaper performance issues. Everything marked **measured** was reproduced on a local PostgreSQL 16
loaded with all 54 migrations and a synthetic dataset (100 properties, 5,000 tenants, 120,000 dues);
single runs on one machine, so treat the numbers as indicative, not as production benchmarks.

### 1. RLS is not wired to the request path (correctness)

Migrations 047/050/053 enable `FORCE ROW LEVEL SECURITY` with a **fail-closed** policy on `tenants`,
`dues`, `payments`, `expenses`, `payout_payees`. A row is visible only if
`app.current_property_id` equals its `property_id` (or `app.ledger_maintenance = 'on'`).

Findings from the code:

- `cmd/server/main.go` builds every repository from the raw `*pgxpool.Pool`.
- `postgres.ScopedDB` (the wrapper that sets the GUC) is referenced by **no production code**.
- The GUC is only set inside `WithinTx`, and only when the context carries a property id. The only
  place that attaches one is the calendar-feed handler. Authenticated API requests and all cron
  jobs never do.
- `app.ledger_maintenance` is set only in tests.
- No migration or doc defines a database role for the application.

**Measured** (non-superuser `NOBYPASSRLS` role): with no scope set, `SELECT count(*) FROM dues` and
`tenants` return **0 rows**; with scope set, 1,200 rows (one property).

Consequence: either the app connects with a role that bypasses RLS (superuser, owner with
`BYPASSRLS`; I believe Neon's default owner role has `BYPASSRLS` but have not verified against your
project: run `\du` on it), in which case **RLS is currently not protecting anything**, or the app
would return empty results the moment it is moved to a least-privilege role. Either way the
protection described in ADR-015 is not in effect today.

### 2. The policy shape defeats the planner (performance)

The policy is `property_id = <scoped uuid> OR current_setting('app.ledger_maintenance') = 'on'`.
An `OR` with a non-column branch cannot become an index condition, so it is applied as a per-row filter.

**Measured**, scoped role, query with no explicit `property_id` predicate:

| Policy | Plan | Rows scanned / removed | Time |
|---|---|---|---|
| Current (`... OR maintenance GUC`) | Index Scan on `idx_dues_due_date` + Filter | 10,000 / 9,900 | 9.76 ms |
| Role-targeted (no `OR`) | Bitmap Index Scan, policy as Index Cond | 147 / 0 | 0.59 ms |

With an explicit `property_id = $1` predicate the current policy costs 0.33 ms vs 0.12 ms with RLS
bypassed (about 2.8x, small in absolute terms). The unscoped-predicate case grows with table size
because it scans every pending due across all properties.

### 3. Cheaper issues

- **Redundant indexes (7):** three duplicate an inline `UNIQUE` index; four are left-prefixes of a
  wider btree. Each costs write amplification and cache for zero read benefit.
- **Autovacuum defaults on queue-like tables** (`outbox_events`, `ledger_outbox_events`,
  `notification_deliveries`, `otp_requests`, `refresh_tokens`, `payment_tokens`): default
  `scale_factor = 0.2` lets dead tuples accumulate and degrades the partial "pending" indexes.
- **N+1 in `reminder`:** one tenant query and up to two property queries per due.
- **`daily-rollup` is orphaned:** `cmd/daily-rollup` and `internal/jobs/daily_rollup.go` exist, but no
  timer or workflow runs them. Its table currently has no readers outside the repository.
- **Duplicate scheduling:** `billing-cycle` and `reminder` are defined both as systemd timers and as
  GitHub Actions crons (Actions is the live one; no server exists yet).
- **Job binaries are never shipped:** `deploy.yml` builds only `server` (and runs `migrate`). None of
  the 10 systemd units' `ExecStart=/opt/pg-app/<job>` binaries would exist on the host.

---

## Decision

1. **(Implemented)** Migration `054_index_dedup_and_autovacuum.sql`: drop the 7 redundant indexes;
   set aggressive autovacuum on the 6 churny tables. Applied and verified on a real PG16 chain run.
2. **(Needs sign-off)** Make RLS real, in this order:
   1. Create two roles: `pgapp_app` (`NOSUPERUSER NOBYPASSRLS`) for API traffic and `pgapp_maintenance`
      (`NOBYPASSRLS`, used by cron jobs, the ledger-outbox worker, notification dispatcher and migrations).
   2. Replace the `OR`-policy (migration 055) with **role-targeted policies**: `... TO pgapp_app USING
      (property_id = <scoped uuid>)` and `... TO pgapp_maintenance USING (true)`. The measured table above
      shows this is both safer (no GUC that any code path can flip) and faster.
   3. Add auth middleware that attaches the property id from verified JWT claims to the request context,
      and route repository access through one executor that sets `set_config(..., true)` **in the same
      round trip as the query** (a `pgx.Batch` runs as one implicit transaction; confirm in the spike).
      Delete or rewrite `ScopedDB`: its current form costs 3-4 round trips per query and uses a
      session-level `set_config(..., false)` on the `Exec` path, which is unsafe under transaction pooling.
   4. Only then switch `DATABASE_URL` to `pgapp_app`; point jobs and background workers at the
      maintenance role. Do this first on a Neon branch of production data.
3. **(Needs sign-off)** Ship job binaries in `deploy.yml` (build all `cmd/*` job targets, atomic-move each
   to `/opt/pg-app/`), so the systemd units can actually run.
4. **(Implemented)** `reminder`: per-run memoization of tenant and property lookups plus an optional
   one-query bulk tenant prefetch (`TenantRepo.GetByIDs`). Test stubs that only implement `GetByID` are
   unaffected. Remaining N+1s: `billing-cycle` (per-tenant `CreateRentDue`) and per-property loops in
   `daily-rollup` / `kpi-snapshot` / `financial-summary`. Batch these when tenant count exceeds the low thousands.
5. **(Implemented)** Add `pg-daily-rollup` systemd service/timer (23:55 IST, because the job rolls up the
   *current* IST day), register it in `install.sh`, and gate the duplicated GitHub Actions crons behind the
   repo variable `SYSTEMD_SCHEDULER` (unset = unchanged behaviour; set to `true` at cutover).

## Options Considered (RLS wiring, decision 2)

### Option A: Keep status quo
| Dimension | Assessment |
|---|---|
| Complexity | None |
| Cost | Zero now |
| Scalability | Neutral |
| Team familiarity | n/a |

**Pros:** no work. **Cons:** RLS is decorative while connected as a bypass role, and a trap if the role
ever changes; ADR-015's tenant-isolation claim is untrue in practice.

### Option B: Drop RLS, rely on application-layer scoping
| Dimension | Assessment |
|---|---|
| Complexity | Low |
| Cost | Low |
| Scalability | Best (no per-row policy cost) |
| Team familiarity | High |

**Pros:** simplest, fastest, honest about what is enforced. **Cons:** loses database-level
defense-in-depth against a missed `WHERE property_id`; every query must be reviewed forever.

### Option C: Role-targeted RLS + request-scoped transaction (chosen direction)
| Dimension | Assessment |
|---|---|
| Complexity | Medium-High (roles, middleware, executor, two pools) |
| Cost | A few engineering days plus staging verification |
| Scalability | Good (policy becomes an index condition; measured 16x on the RLS-only query) |
| Team familiarity | Medium |

**Pros:** real isolation, fail-closed, planner-friendly, maintenance is a role not a flippable flag.
**Cons:** most work; every code path (jobs, workers, webhooks with no user) needs an explicit scope or the
maintenance role; mistakes show up as empty results rather than errors.

## Trade-off Analysis

The honest comparison is **B vs C**. At 5k-12k tenants, throughput is not the constraint (see below),
so the choice is purely isolation-vs-simplicity. Because this system holds rent, deposits and payouts for
multiple future owners (see the multi-owner SaaS roadmap), a missed filter is a cross-customer data leak,
which justifies C. If the product stays single-operator, B is the more defensible choice, and Option A
should be rejected either way.

## Load Estimation (assumptions, not measurements)

Assumed target: 200 properties x 60 tenants = 12,000 tenants. Roughly 12k dues/month, ~14k payments/month,
~4 ledger lines per payment (~0.7M journal rows/year), ~1.7M event rows/year. If 40% of payments land in
the 5 days around due dates, peak is on the order of a thousand payments/day, i.e. single-digit
requests/second. A single PostgreSQL primary handles this with large headroom.

Therefore the read replicas and shard router from ADR-017 are not needed for capacity in this range. Keep
them dormant. **The practical constraints are per-request round trips (latency) and correctness, not throughput.**

## Consequences

- **Easier:** fewer write-path indexes; outbox tables stay compact; reminder runtime stops scaling with
  per-due queries; jobs can be scheduled and shipped from one place; RLS (once Decision 2 lands) is both
  enforced and index-friendly.
- **Harder:** Decision 2 touches every path that talks to the database; two roles and two pools to operate.
- **Revisit when:** p95 query time > 100 ms (the existing slow-query tracer logs at that threshold);
  `events` / `financial_journal_entries` exceed ~50M rows (then partition by month and add retention);
  connections > 70% of `max_connections`; `billing-cycle` runtime exceeds its window (batch it);
  tenants exceed ~50k (revisit replicas).

## Action Items

1. [x] Migration 054 (index dedup + autovacuum), verified by applying 001-054 on PostgreSQL 16.
2. [x] `reminder` N+1 removal (`TenantRepo.GetByIDs`, per-run memoization).
3. [x] `pg-daily-rollup` timer + installer + README; gate duplicate crons with `SYSTEMD_SCHEDULER`.
4. [ ] Check which DB role production uses (`\du`; is it `BYPASSRLS`?).
5. [ ] Decide B vs C. If C: roles, migration 055 (role-targeted policies), middleware, scoped executor,
       staging on a Neon branch, then switch `DATABASE_URL`.
6. [ ] Extend `deploy.yml` to build and install all job binaries.
7. [ ] Run `go test ./...` and `scripts/loadtest` against a staging DB; none of the Go changes in this
       change set were compiled (the sandbox lacked the Go 1.26 toolchain).
8. [ ] Enable `pg_stat_statements`; after 2 weeks review `pg_stat_user_indexes` (`idx_scan = 0`) for more
       unused indexes (after 054 the schema still has 168 explicit `CREATE INDEX` statements across 97
       tables; 318 indexes in total counting PK/UNIQUE, per `pg_indexes` on a fresh PG16).
