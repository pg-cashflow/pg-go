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

| # | Sev | Finding | Evidence | Recommendation |
|---|-----|---------|----------|----------------|
| F1 | High | Dual write (Context item 5): orphan and missing postings are possible. | `NewFinanceRepo(pool)`; `MirrorPayment` runs inside the webhook `WithinTx` closure but commits independently. | **R-1**: write a `payment_mirror` event to `ledger_outbox_events` in the **same transaction** as the payment, and let the worker post idempotently (unique key already exists). Interim: call `Mirror*` after the transaction commits, so the only failure mode is a detectable gap, never an orphan. |
| F2 | High | A refund spanning several dues loses every allocation after the first. `MirrorRefund` is called once per allocation with the same `refundID` and the same `line_kind`s; `uq_journal_source_line` rejects the second insert and Go maps that to "already posted". | Loop in `handlers_pay_refunds.go`; reproduced at DB level (test `REFUND 2nd allocation ... 23505`). Detected by `ledger_refund_posting_gaps`. | **R-2**: post one entry per allocation using `refund_allocations.id` as the source id (a natural idempotency key that already exists), or aggregate by account before posting. Backfill gaps with correcting entries from the detective output. |
| F3 | High | `MirrorPayment` classifies the **whole** payment by the **first** due's kind (`due0 = snapshot[0]`), though the webhook allocates it across several dues. A combined deposit + rent payment is booked entirely as liability or entirely as revenue. Overpayments credited to the tenant's balance are also booked as revenue; there is no account for tenant credit. | `handlers_pay_webhooks.go:185,313`; no `credit_balance` reference in `internal/finance`. | **R-4**: post per allocation (`payment_allocations`) by due kind, and journal the overpayment to a liability account (`tenant_credit` / reuse `unapplied_receipts`). Add an account to `ledger_account_class` in the same migration. |
| F4 | Medium | Cash, manual and auto-matched payments created by `internal/payment` never reach the ledger; the only `MirrorPayment` callers are the Cashfree webhook paths. Bank-statement credits have their own posting path. | grep: no `Mirror`/`Finance` reference in `internal/payment`. | Confirm intent. If the ledger is meant to be complete, post from `settleMatched`. Today the tie-out compares total collections to `rent_revenue`, so these appear as a difference and block close. `ledger_unposted_payments` lists them (reported, not auto-escalated). |
| F5 | Low (latent) | `MirrorProration` credits `tenant_receivable`, which the design note says is never debited at due creation; it would drive the account negative and reduce revenue never recognised. It has **no callers** today. | `mirror.go`; grep shows no call sites. | Remove or redesign before wiring it up. |
| F6 | Medium | The override GUCs are advisory. | See Consequences. | **R-3**: replace the GUC with `SECURITY DEFINER` functions (`ledger_reopen_period(property, period, reason)`) executable only by a `ledger_admin` role, `REVOKE` direct writes from the app role, and expose reopen as an owner API behind the existing step-up OTP (Track C.3) with a mandatory reason. |
| F7 | Info | `gofmt -l` reports drift in several untouched files (e.g. `domain/finance.go`, `apierr/codes.go`). | `gofmt -l`. | Add a `gofmt -l` gate to CI in a separate, formatting-only commit so it does not mix with logic changes. |

---

## Action Items

1. [ ] Run `ledger_preflight_audit.sql` against staging, review every violation with the Finance Domain Owner **before** go-live.
2. [ ] Apply 044 to staging; run `ledger_controls_test.sql` there (it rolls back and is safe on dev/staging, never production).
3. [ ] Run `go build ./... && go test ./...` (the sandbox that produced this change could only build the `domain` and `finance` packages; see the PR description).
4. [ ] Decide F4 (should cash/manual payments post?) and F3/F2 priorities; schedule R-1, R-2, R-4.
5. [ ] Schedule R-3 (privileged reopen) before multi-owner SaaS.
6. [ ] Wire `ledger_reconciling_items` into the existing alerting so `LEDGER GAP` logs are no longer the only signal.
