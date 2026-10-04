# Ledger Controls Matrix and Test Workpaper

**Scope:** the general ledger (`financial_journal_entries`), period close (`period_tie_outs`) and the payment-to-ledger interface of pg-go.  
**Basis:** the COSO / SOX 404 structure (risk, control, assertion, test, deficiency) used as a rigour framework. SOX applies to US-listed companies; pg-go is an Indian proprietorship, so **nothing here is a statement of SOX or statutory compliance.** Reporting is on a collection basis (see `docs/finance/journal-entry-catalogue.md`).  
**Version:** 2026-10-04, migrations 043-044, ADR-015/016.

## 1. Assertions in scope

| Assertion | Meaning for this ledger |
|-----------|-------------------------|
| Completeness | Every payment, refund and payout that happened is in the journal |
| Existence / occurrence | Every journal entry corresponds to something that happened (no orphans) |
| Accuracy | Amounts and the debit/credit sides are right (integer paise, entries balance) |
| Classification | Amounts land in the right account (deposit liability vs revenue) |
| Cut-off | Entries fall in the correct period; closed periods stay closed |
| Presentation | Statements derive from the journal and tie to each other |

## 2. Control matrix

Type: **P** preventive, **D** detective. Mode: **A** automated, **M** manual.

| ID | Risk | Control | Type / Mode | Frequency | Owner | Assertion | Evidence |
|----|------|---------|-------------|-----------|-------|-----------|----------|
| L-01 | Posted entries altered or removed | `trg_journal_append_only`: UPDATE/DELETE rejected (`LG003`); DELETE only with `app.ledger_maintenance`, and logged | P / A | Every write | Engineering | Accuracy, existence | Trigger definition; test checks `C-1` |
| L-02 | One-sided or mis-sided entry | `trg_journal_balanced`: per source **and property**, debits = credits at COMMIT (`LG002`) | P / A | Every transaction | Engineering | Accuracy | Trigger definition; test checks `C-2` |
| L-03 | Closed period rewritten | `trg_tieout_closed_frozen` (`LG004`); reopen audited | P / A | Every write | Engineering | Cut-off | Test checks `C-3` |
| L-04 | Posting into a signed-off period | `trg_journal_period_lock` (`LG001`), UTC months | P / A | Every write | Engineering | Cut-off | Test checks `C-4` |
| L-05 | Override used without accountability | `ledger_control_overrides` immutable log; **review log at each close** | D / A+M | Monthly | Owner (+ reviewer) | Existence, cut-off | Query output retained with close pack |
| L-06 | Close with unexplained difference or open period | `CloseTieOut`: difference must be 0 and the period must have ended | P / A | Each close | Owner | Cut-off, accuracy | Go tests `TestCloseTieOut*` |
| L-07 | Ledger and payments diverge (dual write) | `ledger_unposted_payments`, `ledger_orphan_postings`, `ledger_refund_posting_gaps` feed `ledger_reconciling_items` | D / A+M | At least monthly; recommend daily | Owner | Completeness, existence | Query output; zero or explained |
| L-08 | Stale unreconciled items | Aging review of `ledger_reconciling_items`, escalation tiers | D / M | Monthly | Owner | Completeness | Output + disposition notes |
| L-09 | Statements do not tie | `check_difference` = 0 on balance sheet and cash flow; net income agrees | D / A+M | Each close | Owner | Presentation, accuracy | Statement exports |
| L-10 | Historical data violates rules | `ledger_preflight_audit.sql` | D / A | Before deploy, monthly | Engineering | All | Script output |
| L-11 | Uncontrolled schema/logic change | Migration checksum guard (edited applied migration is rejected); regression suite; ADR per control change | P / A+M | Each change | Engineering | All | Runner behaviour; PR record |
| L-12 | One person prepares and approves | Segregation: preparer is not the approver of closes/overrides | P / M | Each close | Owner | All | **Not operating** while there is a single operator (see 5) |

## 3. Test procedures and sample sizes

**Automated controls (L-01 to L-04, L-06, L-10).** Test of design plus a test of one instance, then rely on change control (L-11) between releases: re-test only when the trigger, migration, or the relevant Go code changes. Executable procedure: `scripts/sql/ledger_controls_test.sql` (rolls back, never run on production).

**Manual / semi-manual recurring controls (L-05, L-07, L-08, L-09).** Standard guidance for a monthly control is 2-5 occurrences per period; at 12 occurrences a year, test **all closed months** (100%) while the population is this small. Per month, the reviewer re-performs: run the query, confirm zero or documented items, confirm dated sign-off, and for L-05 reconcile each override row to an approved reason.

**Re-performance, not inquiry.** For L-07 and L-09 the tester re-runs the queries themselves against the database and compares to the retained output.

## 4. Test of design and results (performed 2026-10-04)

Environment: PostgreSQL 16, all migrations 001-044 applied from empty and as an upgrade over data created under 043.

| Control | Procedure | Result |
|---------|-----------|--------|
| L-01 | UPDATE, DELETE rejected (`LG003`); DELETE with maintenance flag allowed and logged (3 rows, actor recorded) | Pass |
| L-02 | Single unbalanced line rejected at commit; cross-property netting entry rejected (the 043 gap); multi-line balanced entry accepted | Pass |
| L-03 | Upsert and DELETE on a closed period rejected; reopen with flag allowed and logged as `period_reopened` | Pass |
| L-04 | Posting into a closed month rejected, including the last UTC second; first instant of next month accepted; `03:00 IST` on the 1st correctly treated as the prior UTC month; another property unaffected; override posting logged; accepted again after reopen | Pass |
| L-05 | Audit log UPDATE/DELETE rejected, **even with the maintenance flag** | Pass |
| L-06 | Go tests: cannot close before period end (mid-period and last second), exactly-at-end allowed, idempotent, unexplained difference still blocks, closed period returns frozen snapshot | Pass |
| L-07 | Fixtures with an unposted gateway payment, an orphan posting, and a refund journaled for 6,000 of 10,000: all three detected; in-flight rows inside the grace window ignored; cash payment reported but not auto-escalated; each repair path (runbook 6.1-6.3) clears its item | Pass |
| L-08 | Aging buckets, timing vs investigate, and escalation tiers asserted on constructed items | Pass |
| L-09 | One realistic month: net income 148,500; balance-sheet and cash-flow `check_difference` = 0; period scoping (August isolated); closing cash = bank balance | Pass |
| L-10 | Legacy data with a cross-property source, a legacy unbalanced row and an unknown account: all flagged, non-zero exit; clean database exits 0 | Pass |
| Suite quality | Every control was **mutation-tested**: each was removed or weakened in a scratch database and the suite failed (11 mutations of the SQL controls and 3 of the Go tie-out logic, all caught) | Pass |
| Regression | 65 SQL checks; repeated under three session time zones (UTC, UTC+14, UTC-8) with identical results | Pass |

**Not tested, and why:**
- **Operating effectiveness of manual controls (L-05, L-07, L-08, L-09, L-12):** they have not yet operated for a real month; first test is after the first close performed under this runbook.
- **Go code outside the `domain` and `finance` packages** (repo mapping of `LG001`/`LG004`, API 409 mapping, webhook comment change): could not be built in the authoring environment (it needs Go 1.26.6 and modules unavailable offline). They are small, `gofmt`-checked, and covered by the existing `error_characterization` and contract-parity tests, which **must be run in CI** before merge.
- **Production data:** the pre-flight audit has not been run against production. Its results determine whether any finding below is quantitatively significant.

## 5. Deficiency evaluation

Severity = reasonable possibility of a misstatement x potential magnitude, after compensating controls. Quantification needs the pre-flight audit on production data; until then these are **provisionally** classified.

| # | Deficiency (ADR-016) | Likelihood | Magnitude | Compensating control | Provisional class | Remediation |
|---|----------------------|-----------|-----------|----------------------|-------------------|-------------|
| F1 | Dual write between payment and journal | Reasonable possibility (any DB error at the wrong moment) | Per-payment, accumulates | L-07 detective (monthly or better) | **Significant deficiency** until outbox (R-1) or post-commit mirroring ships | R-1 |
| F2 | Multi-due refunds lose later allocations | Reasonable whenever a refund spans dues | Per-refund | L-07 refund-gap detection | **Significant deficiency**; becomes a fix-now item if the audit finds any gap | R-2 |
| F3 | Whole payment classified by first due; overpayments booked as revenue | Reasonable whenever one payment covers mixed dues or overpays | Can shift deposit liability into revenue | None (not detected) | **Significant deficiency**: no compensating control exists; confirm magnitude on production | R-4 |
| F4 | Cash/manual payments not posted | Certain if the ledger is meant to be complete | Potentially all cash collections | Tie-out difference blocks close; `ledger_unposted_payments` lists them | **Needs a product decision first**; classify after | decision |
| F6 | Overrides are advisory (GUC) | Low with one trusted operator | High if abused | L-05 immutable log and review | Control deficiency | R-3 |
| L-12 | No segregation of duties | Inherent for one operator | n/a | Immutable override log | Accepted limitation; revisit when a second person has access | when staffing changes |

**Aggregation:** F1, F2 and F3 affect the same assertion family (completeness and classification of collections) and should be evaluated together. Together they are the principal risk to the reliability of reported revenue; treat the group as a **potential material weakness** for revenue reporting if the production audit shows non-trivial amounts. This is a judgement for the Finance Domain Owner, not a conclusion reached here.

## 6. Evidence retention

For each monthly close retain: the four statement exports, `ledger_reconciling_items` output with dispositions, the `ledger_control_overrides` extract for the month with sign-off, and the pre-flight audit output. Store with the close pack; keep for the period your accountant advises.
