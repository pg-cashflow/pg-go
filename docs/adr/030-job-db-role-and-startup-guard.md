# ADR-030: Background Jobs Connect as `pgapp_worker` With a Startup Role Guard

**Status:** Proposed
**Date:** 2026-10-14
**Deciders:** Sasi (owner/engineer)
**Builds on:** ADR-018, ADR-020, ADR-029, migrations 058, 065, 069

## Context

- Migration 069 creates `pgapp_worker` "for background tasks and webhooks".
- No Go code connects as it. All 9 recurring jobs call `postgres.NewPool(ctx, cfg.DatabaseMaintURL)`.
- Only `cmd/server` runs `ValidateStartupRoles`. A job started in production with a superuser or table-owner URL would run without any check.
- `pgapp_worker` and `pgapp_maint` have the same grants and the same RLS policies today (065:15 and 069:32-39, 82, 109). Both lack `UPDATE`/`DELETE` on the ledger tables.
- `DATABASE_MAINT_URL` falls back to `DATABASE_URL` (`config.go:85`), so a missing variable silently reuses the app role.

## Decision

Option B: wire jobs to a dedicated worker credential and guard it.

1. Add `Config.DatabaseWorkerURL` from `DATABASE_WORKER_URL`.
   - Production: required, and must differ from `DATABASE_URL` and `DATABASE_MAINT_URL`.
   - Non-production: falls back to `DATABASE_MAINT_URL`, with a logged warning.
2. All 9 recurring jobs open `cfg.DatabaseWorkerURL`.
3. Add `postgres.ValidateWorkerRole(ctx, pool, appEnv)`. In production the role must be `pgapp_worker`, `NOSUPERUSER`, `NOBYPASSRLS`, and not a table owner. Each job calls it after `config.Load()` and `ValidateForRealDeployment()`.
4. `cmd/server` keeps `maintPool` for its own paths. ADR-029 D2 (bind `property_id` in the context before scoped work) still applies to jobs.
5. `cmd/migrate` keeps its own unpooled URL and is **exempt** from steps 2-3.

## Options Considered

### Option A: Leave as is
| Dimension | Assessment |
|---|---|
| Complexity | None |
| Security | Jobs share a credential with the API; no guard on jobs |
| Team familiarity | High |

**Pros:** no work. **Cons:** an unused role is dead weight, and a wrong URL goes unnoticed.

### Option B: Wire jobs to `pgapp_worker` (selected)
| Dimension | Assessment |
|---|---|
| Complexity | Low (one config field, one guard, 9 one-line changes) |
| Security | Separate credential, separate `pg_stat_activity` identity, revocable alone, guard on every job |
| Cost | One more connection string; one more role on the host (Neon console or `ALTER ROLE ... PASSWORD`) |

**Pros:** blast-radius and audit separation now; a place to narrow grants later. **Cons:** no privilege reduction on day one.

### Option C: Delete `pgapp_worker`
**Pros:** fewer moving parts. **Cons:** gives up the separation, and 069 already ships it.

## Trade-off Analysis

Because the two roles are equal today, B is about **attribution and containment**, not capability. The real least-privilege gain comes from a follow-up that narrows worker grants per job family (for example `reminder` needs no write on journals). That follow-up is only possible if jobs already use the role. So B is the cheapest path to a real benefit.

## Consequences

- Easier: tracing which process touched a row; rotating job credentials alone; adding per-job grants.
- Harder: one more secret per environment; the deploy runbook (R7) must create the role and password on both a VPS and Neon.
- Revisit: per-job roles; narrowing grants; whether `pgapp_maint` is still needed once ADR-029 D1 routes request paths through `ScopedDB`.

## Action Items

1. [ ] Config field, env example, systemd `EnvironmentFile` note (R5).
2. [ ] `ValidateWorkerRole` with table-driven unit tests for each rejected role shape (R5).
3. [ ] Live test: worker cannot `UPDATE` `financial_journal_entries`; worker can run each job's writes (R6, `REQUIRE_DB=1`).
4. [ ] Follow-up ticket: grant narrowing per job family.
