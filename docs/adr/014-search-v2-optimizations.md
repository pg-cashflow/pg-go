# ADR 014: Search V2 Optimizations — Property-Scoped Trigrams, Prefix Pattern Seeks & Lean Indexing

**Status:** Accepted  **Date:** 2026-10-03  **Decider:** Divakar

## Context
Following the deployment of ADR-012 (Postgres live-table search) and ADR-013 (bounded fan-out and merge observability), scale testing revealed:
1. **Cross-property GIN bloat:** Global trigram indexes on `bank_transactions`, `dues`, and `tenants` caused index scans across hundreds of noise properties, deteriorating query latency at scale (1,000+ properties).
2. **Identifier scan overhead:** Code-like lookups (e.g. UTRs, txn IDs, due codes) suffered high index lookup cost under GIN trigram scans compared to B-tree range queries.
3. **Write amplification:** Maintaining multiple redundant global GIN indexes added significant overhead to bank statement ingestion and payment reconciliation.
4. **Recall regressions:** Earlier query builder adjustments broke suffix fragment recall on transaction IDs (`txn_id`, `upi_txn_id`).

## Decision
1. **Property-Scoped Composite GIN Indexes (Migration 040):**
   - Enabled `btree_gin` extension.
   - Built composite indexes `(property_id, col gin_trgm_ops)` on `dues(property_id, due_code)`, `bank_transactions(property_id, narration)`, and `bank_transactions(property_id, txn_id)`.
2. **B-Tree Prefix Pattern Operations (Migration 041):**
   - Created `text_pattern_ops` indexes: `payments(lower(upi_txn_id) text_pattern_ops)` and `bank_transactions(property_id, lower(txn_id) text_pattern_ops)`.
   - Utilized byte-bounded prefix range seeks (`~>=~` and `~<~` via `prefixRange()`) for ASCII code identifiers, maintaining Index Scan paths without trigram overhead.
3. **Drop Redundant Global Indexes (Migration 042):**
   - Dropped global trigram indexes `idx_bank_transactions_narration_trgm` and `idx_bank_transactions_txnid_trgm`, yielding a verified 36.7% write speedup.
4. **Two-Stage Query Architecture with Gated Fallback:**
   - **Stage 1 (fast):** Exact / prefix B-tree index scan on identifiers or tenant room/name.
   - **Stage 2 (fallback):** Trigram scan gated by `(SELECT count(*) FROM fast) = 0`, evaluated as an `InitPlan` one-time filter so Postgres completely skips expensive fallback scans when Stage 1 succeeds.

## Review Conditions Closed
- **Noise Scale Validation (2.1):** Seeded 100 bank transactions and 100 payments across 999 noise properties (~100k rows each) in `TestPerformanceGateStrictTarget`.
- **Production Query Builder EXPLAIN Plans (2.2):** Validated exact query plans from `repo.buildBankTransactionsQuery` and `repo.buildPaymentsQuery` with `EXPLAIN (ANALYZE, BUFFERS)`. Verified zero sequential scans.
- **Statistically Rigorous Latency Benchmark (2.3):** 3 repeats $\times$ 200 samples per class (2,400 queries total) asserting median p95 $\le 100\text{ ms}$:
  - `fast`: median p95 = **62.06 ms**
  - `slow-narration`: median p95 = **95.41 ms**
  - `slow-txn-fragment`: median p95 = **41.77 ms**
  - `slow-digit-ref`: median p95 = **34.33 ms**
  - Overall partial rate: **0.0%** across all repeats.
- **Safety Guards in Golden Suite (2.4):** Guarded `TestLivePostgresSearchV2_GoldenRelevanceSuite` with `requireDisposableDB` and fail-fast migration execution; all 42 golden relevance assertions pass (8.88s).
- **Fair A/B Write-Cost Measurement (3.0):** Fair 3-trial A/B test with alternating order on 100k fresh rows confirmed **36.7% write speedup** (2.847s vs 4.498s). Real bank CSV statement ingestion timed at **35.8% faster** (1.050s vs 1.637s; 4,760 rows/s vs 3,055 rows/s).
- **Per-Entity Duration Observability (6.0):** Integrated `searchEntityObserver` tracking per-entity p50/p95/max across all queries.
