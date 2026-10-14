# Module: M12 Platform

**Owner:** Divakar · **Sprint:** 1 · **ADR:** ADR-030, ADR-031, ADR-035

## Gate 1 — Journeys

| ID | Actor | Precondition | Action | Expected HTTP | Side effect |
| --- | --- | --- | --- | --- | --- |
| J1 | Operator | Valid env | Start `cmd/server` | 200 on `/healthz` | Pools connect |
| J2 | Operator | Wrong metrics token | `GET /metrics` | 401 | No metrics leak |
| J3 | Operator | `cmd/migrate` | Run migrate | 0 exit | Schema current |
| J4 | Operator | Job env | Run `cmd/billing-cycle` | 0 or fail-fast | `ValidateWorkerRole` in prod |

## RLS D1 follow-ups (source: `docs/wayfinder/RLS_D1_STATUS.md` §3)

| Ticket | Summary | Target sprint |
| --- | --- | --- |
| R9-F1 | RLS + ScopedDB `bank_accounts` | M5 |
| R9-F2 | ScopedDB `bank_transactions` + isolation test | M5 |
| R9-F3 | ScopedDB `payout_batches` + isolation test | M7 |
| R9-F4 | RLS `payout_items` / `payout_records` | M7 |
| R9-F5 | ScopedDB customer reads on journal entries | M6 |
| R9-F6 | RLS `property_calendar_tokens` | M12/M1 |
| R9-F7 | Tenant identity projection / maint pool | M1/M3 |

M12 delivers the mapping only; implementation lands in the target sprint.

## Gate 8 — Exit checklist

- [x] ADR-033 reserved; ADR-034/035 indexed
- [x] Doc grep: remove obsolete legacy search reindex job references
- [x] Jobs use `DATABASE_WORKER_URL` + `ValidateWorkerRole` (ADR-030)
- [ ] `go test -count=1 ./...` green on CI machine
- [ ] `REQUIRE_DB=1` recorded in `BASELINE_REPORT.md` when Neon roles exist
- [ ] VPS provisioning checklist in runbook (optional until Track Q)
