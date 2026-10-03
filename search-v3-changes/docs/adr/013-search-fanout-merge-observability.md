# ADR 013: Search — Global Merge, Concurrent Fan-out, Observability

**Status:** Proposed  **Date:** 2026-10-03  **Deciders:** Divakar

## Context
ADR-012 moved search to live-table Postgres. Review of the code found:
1. Per-type top-5 lists were concatenated in fixed type order and cut at `limit` (default 20). Owners have 13 types, so later types (bank txns, settlements, refunds, payouts) were starved — contradicting ADR-012 §2.6.
2. `pgx.Batch` saves round-trips but runs statements serially in one implicit transaction; one timeout aborts all later statements, so `partial` meant "lost everything after the first failure".
3. Failures were swallowed (no log, no metric); p95/p99 and partial-rate were unknowable.

Scale (ADR-012): 1k+ users, <1 QPS average, a few QPS peak, 100k–500k rows. Latency is not the main risk; **correctness, failure isolation and visibility** are.

## Decision
1. `search.MergeResults`: intent-aware (token kind → preferred entity types) scoring, one guaranteed slot per matching type, then fill by score. Deterministic.
2. Pool-backed repo fans out per-type queries, concurrency capped at 4 per request, 300 ms timeout each, results kept in query order. Batch path retained only for tx / test doubles. All-queries-failed returns an error (HTTP 500) instead of an empty 200.
3. Structured `slog` warnings per failed/slow (>100 ms) entity query. Search pool default 10 conns (`SEARCH_DATABASE_MAX_CONNS`).

## Options considered
| Option | Complexity | Notes |
|---|---|---|
| A. Keep batch | Low | Serial, abort-on-error. Rejected. |
| B. Bounded fan-out (chosen) | Med | Failure isolation; bounded pool pressure. Needs pool ≥ fanout × concurrent searches. |
| C. Unbounded fan-out | Low | 13 conns per owner search would exhaust a small pool. Rejected. |
| D. External engine (OpenSearch/Typesense) | High | Unjustified at <5 QPS, adds sync/staleness + DPDP surface. Revisit > ~50 QPS or >5M rows. |

## Consequences
- Easier: global ranking, partial-failure behaviour, debugging.
- Harder: more connections used per request; merge scoring is heuristic and needs tuning from real click data.
- Revisit: min query length 3 (trigram limit), `btree_gin (property_id, col gin_trgm_ops)` composites, read replica, response cache, locale of production DB for Telugu/Hindi.

## Action items
1. [ ] Run `go vet`/`go test ./...` and the live + perf-gate tests on a real Postgres (not run in this change).
2. [ ] `SHOW lc_ctype;` on prod — pg_trgm ignores non-ASCII under `C` locale.
3. [ ] Load test: `k6 run scripts/loadtest/search.js` at 100–300 VUs; record p95/p99, 429 rate, partial rate.
4. [ ] Frontend: 250 ms debounce + AbortController; raise search rate limit (burst ≥ 20, ≥ 2 rps) once debounced.
5. [ ] Server cache 5–10 s keyed (role, property, tenant, normalized q) with singleflight.
6. [ ] Cloud Run: pool conns × max instances ≤ DB max_connections; consider PgBouncer / read replica.
7. [ ] Alerts: search p95 > 300 ms, partial rate > 2 %, 5xx > 1 %.
