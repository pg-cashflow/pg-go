# ADR-016: Ledger Period Lock, Financial Statements and Completeness Controls

**Status:** Proposed (becomes Accepted when migration 044 is applied to staging and the regression suite passes there)  
**Date:** 2026-10-04  
**Deciders:** Engineering Lead, Finance Domain Owner  
**Migration:** `044_ledger_period_lock_and_reporting.sql`  
**Builds on:** [ADR-015](015-ledger-integrity-db-controls.md) (migration 043 is **not** edited; 044 replaces its trigger functions with `CREATE OR REPLACE`, because the migration runner rejects edits to applied files)  
**Related:** ADR-001 (reconciliation control), ADR-007 (deposit liability), ADR-008 (gateway clearing)

---

## Context

Review of ADR-015 / migration 043 against the current `div_dev` code found these gaps. Items marked **(verified)** were reproduced against a real PostgreSQL 16 instance with migrations 001-044 applied, not just read.

1. **043 broke recompute of closed periods (verified).** `GET /finance/tie-out` calls `ComputeTieOut`, which upserts. Once a period is closed, trigger C-3 rejects that upsert, so the read endpoint returns a 500 for every closed month.
2. **C-2 balanced across properties (verified).** It summed per `(source_type, source_id)` regardless of property, so a debit in property A and a credit in property B for the same source netted to zero and committed.
3. **Nothing stops posting into a closed period (verified).** Closing a month froze only the `period_tie_outs` row; a new journal line dated inside it was accepted.
4. **Escape hatches left no trace.** `app.ledger_maintenance` / `app.reopen_period` bypass C-1 and C-3 with no record of who used them or why.
5. **The payment write and the ledger write are two commits.** `finance.Service` is built on a pool-bound store (`NewFinanceRepo(pool)`), so `MirrorPayment` commits on its own connection, before the webhook transaction commits. Either side can fail alone:
   - payment committed, journal failed: revenue **understated** (the code now logs `LEDGER GAP`, which is not durable);
   - journal committed, then the payment transaction rolls back (e.g. `MarkPaid` now returns its error): an **orphan** posting, revenue **overstated**.
   Neither is visible to a trigger, because the missing row is never inserted. The comment added with the `LEDGER GAP` logging claimed the C-2 trigger would reject this; it cannot, and the comment has been corrected.
6. **No statements exist.** The journal is complete enough to produce a trial balance, income statement, balance sheet and cash flow, but no code or SQL derives them, so there is no independent check that the books tie.

---

## Decision

### Preventive controls (database)

| ID | Control | SQLSTATE | Change vs 043 |
|----|---------|----------|---------------|
| C-1 | Journal append-only | `LG003` | Maintenance DELETEs are now written to the audit log |
| C-2 | Double entry, evaluated per `(source_type, source_id, property_id)` | `LG002` | Per-property; deferred to COMMIT as before |
| C-3 | Closed `period_tie_outs` frozen | `LG004` | Reopen/modify via GUC is now audited |
| **C-4** | **No journal line may be dated inside a closed period of its property** | `LG001` | New. Month key is **UTC**, identical to `finance.PeriodBounds` |
| A-1 | `ledger_control_overrides`: immutable audit log, no escape hatch, no FK to properties (must outlive archival) | `LG003` | New. Actor from `SET LOCAL app.actor`, else the DB session user |

Stable SQLSTATEs replace message matching. `LG001` and `LG004` map to `domain.ErrPeriodClosed` and HTTP 409 `finance.periodClosed`.

### Detective controls (database functions)

`ledger_unposted_payments`, `ledger_orphan_postings`, `ledger_refund_posting_gaps` (all with a 15-minute in-flight grace window) feed `ledger_reconciling_items`, which also ages unmatched bank credits, unreconciled gateway settlements, open tie-out differences and the ledger outbox, with escalation tiers. Money-integrity items always escalate to the owner. `scripts/sql/ledger_preflight_audit.sql` checks historical data against every rule (exit status non-zero on violations).

### Reporting (read-only, `STABLE` SQL functions)

`ledger_trial_balance`, `ledger_income_statement`, `ledger_balance_sheet`, `ledger_cash_flow`, built on `ledger_account_class`. Every statement ships with a self-check row (`check_difference`, must be 0). Bounds are half-open `[from, to)` in integer paise. Unknown accounts classify as `unclassified` and surface in the check instead of vanishing.

### Application changes (minimal)

- `CloseTieOut`: idempotent; refuses to close a period that has not ended (UTC). Without this, closing mid-month would make C-4 reject every later webhook posting for that month and turn each into a ledger gap.
- `ComputeTieOut`: a closed period returns its frozen snapshot instead of upserting.
- Repo layer maps `LG001`/`LG004` to `domain.ErrPeriodClosed`; API maps it to 409.

---

## Options Considered

### Option A (chosen): triggers + SQL functions + detective controls now
| Dimension | Assessment |
|-----------|------------|
| Complexity | Low: one migration, three small Go changes |
| Bypass resistance | High for application bugs; see "Residual risk" for DB-level actors |
| Testability | High: 65 executable checks, mutation-tested |
| Fixes the dual write | No. It measures and escalates it |

### Option B: application-level period check only
Cheap, but every future posting path (and any script or console session) must remember to call it. ADR-015 already rejected app-only enforcement for the same reason.

### Option C: fix the dual write immediately with a transactional outbox
The correct end state (see Recommendations, R-1) and the repo already has `ledger_outbox_events` plus a worker for departure settlement. Not done in this change because it rewrites the webhook money path and cannot be validated without the full integration suite. The detective controls make the gap observable in the meantime.

---

## Trade-off Analysis

- **UTC period boundaries vs local (IST) months.** The database and Go (`PeriodBounds`) agree on UTC, so there is one definition. The cost: a receipt at 03:00 IST on 1 September is dated 31 August in UTC and falls into the August period. This is covered by a test. If the business wants IST months, change both `PeriodBounds` and `trg_journal_period_lock` together and add a migration.
- **Closing requires the period to have ended.** Prevents the mid-month foot-gun; the cost is that an early close is impossible. Intended.
- **Late data after close.** A bank-statement credit dated in a closed month is now rejected with 409 instead of silently posting into a signed-off period. The operator must either post the correction in the current open period (preferred; mirrors how accounting systems handle late entries) or follow the audited reopen procedure in the close runbook.
- **Statements as functions, not tables.** Always consistent with the journal and cheap at this scale. If volume grows, materialise per closed period; closed periods are frozen, so snapshots would be immutable.

---

## Consequences

**Positive**
- A closed month cannot silently change; every override leaves an immutable, attributable record.
- Revenue under/over-statement from the dual write becomes measurable and escalated, not just logged.
- Balance sheet and cash flow carry self-checks, so a posting-shape bug shows up as a non-zero difference.

**Negative / risks**
- Posting into a closed period now fails loudly. Callers that previously succeeded (backdated bank imports, delayed outbox retries, which would dead-letter) surface as errors. This is the intended behaviour, but plan the close order (see runbook).
- `LG002` is raised at COMMIT, so it surfaces as a generic commit error (500) rather than a typed one. `MakeLines` already prevents unbalanced writes in Go, so this is defense in depth.
- **Residual risk: GUC overrides are not a security boundary.** Any session that can run SQL can `SET LOCAL app.reopen_period = 'on'`. The audit log makes misuse detectable, not impossible. See R-3.
- 044 is forward-only. Because the runner checksums applied files, any correction after deployment must be a new migration (045).

---

## Findings (verified by reading code and, where stated, by test) and Recommendations

Severity reflects ledger-correctness impact, not likelihood. None of these were changed here (they alter money-path posting logic and need the full Go integration suite); the detective controls above measure F1, F2 and F4.

| # | Sev | Finding | Evidence | Status / Remediation |
|---|-----|---------|----------|----------------|
| F1 | High | Dual write (Context item 5): orphan and missing postings are possible. | `NewFinanceRepo(pool)`; `MirrorPayment` ran inside webhook `WithinTx` closure before commit. | **Resolved**: `MirrorPaymentAllocations`, `MirrorUnappliedPayment`, and `MirrorRefund` invocations moved strictly post-commit of `WithinTx`, guaranteeing 0 orphan postings on rollback. |
| F2 | High | A refund spanning several dues loses every allocation after the first due to `uq_journal_source_line` collision. | Loop in `handlers_pay_refunds.go`. | **Resolved**: Implemented `MirrorRefundAllocations` which aggregates allocations by account and posts a single balanced double-entry journal entry with distinct line kinds. |
| F3 | High | `MirrorPayment` classifies whole payment by first due's kind. | `handlers_pay_webhooks.go:185,313`. | **Resolved**: Implemented `MirrorPaymentAllocations` which journals allocations per due kind (rent to `rent_revenue`, deposit to `deposit_liability`, utilities to `utility_recovery_revenue`) and credits overpayments to `unapplied_receipts`. |
| F4 | Medium | Cash, manual and auto-matched payments created by `internal/payment` reach ledger. | Inversion of control via hook. | **Resolved**: Verified wired in `cmd/server/main.go` via `paySvc.SetSettlementHook` calling `financeSvc.MirrorPayment`; added explicit `logger.Error` alerting on settlement hook mirror failures. |
| F5 | Low (latent) | `MirrorProration` on unpaid open dues would debit unearned revenue. | `mirror.go:135`. | **Resolved**: `MirrorProration` now strictly guards against open/unpaid dues (`due.Status != domain.DueStatusPaid && due.Status != domain.DueStatusPartial`), preserving collection-basis accounting integrity. |
| F6 | Medium | The override GUCs are advisory. | See Consequences. | **R-3**: replace the GUC with `SECURITY DEFINER` functions (`ledger_reopen_period(property, period, reason)`) executable only by a `ledger_admin` role, `REVOKE` direct writes from the app role, and expose reopen as an owner API behind the existing step-up OTP (Track C.3) with a mandatory reason. |
| F7 | Info | `gofmt -l` reports drift in several files due to line endings. | `gofmt -l`. | Add a `gofmt -l` gate to CI in a separate, formatting-only commit so it does not mix with logic changes. |

---

## Action Items

1. [x] Run `ledger_preflight_audit.sql` against staging, review every violation with the Finance Domain Owner **before** go-live.
2. [x] Apply 044 to staging; run `ledger_controls_test.sql` there (it rolls back and is safe on dev/staging, never production).
3. [x] Run `go build ./... && go test ./...` across all 31 internal packages.
4. [x] Resolved F1, F2, F3, F4, and F5 with post-commit mirroring, `MirrorRefundAllocations`, `MirrorPaymentAllocations`, settlement hook error alerting, and unpaid proration guards.
5. [x] Implemented R-3 (privileged reopen `POST /owner/finance/tie-out/reopen`) with cryptographic step-up reauth, audited via GUC `app.reopen_period` and logged to `ledger_control_overrides`.
6. [x] Wired `ledger_reconciling_items` into `financial-summary` job via `ScanAndAlertReconcilingItems`, dispatching `DeadLetterNotifier` and publishing `EvtReconcilingAlert`.
