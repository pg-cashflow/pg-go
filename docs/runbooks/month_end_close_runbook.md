# Month-End Close, Reconciliation and Ledger Repair Runbook

Applies to migrations 043/044 ([ADR-015](../adr/015-ledger-integrity-db-controls.md), [ADR-016](../adr/016-ledger-period-lock-statements-completeness.md)). All periods are **UTC calendar months** (`YYYY-MM`). Amounts are paise (divide by 100 for INR).

**Roles.** Today one operator prepares and approves. When a second person exists, the preparer must not be the approver of the close or of any override (maker-checker; see the control matrix).

**Never run `ledger_controls_test.sql` against production.** It rolls back, but it is a test harness.

---

## 1. Timeline

| When | Step |
|------|------|
| Month end | Nothing closes yet. The period must have ended (UTC) before it can close. |
| T+1 to T+3 | Import bank statements and gateway settlements for the month; run payout batches; resolve unmatched items (section 2). |
| T+3 to T+5 | Review statements (section 3), close (section 4). |
| After close | Late items go into the **current open period** (section 5). |

Close only after statement imports are done: a bank credit dated inside a closed month is rejected with `finance.periodClosed` (HTTP 409).

## 2. Reconcile (before closing)

```sql
-- Everything still open, oldest first. escalation: none | manager | owner
SELECT item_type, ref, amount_paise, age_days, age_bucket, category, escalation
  FROM ledger_reconciling_items('<property_uuid>');
```

| `item_type` | Meaning | Action |
|-------------|---------|--------|
| `bank_credit_unapplied` | Bank credit not matched to a due | Match, refund, or reclassify in the app |
| `gateway_settlement_unreconciled` | Settlement unmatched or in discrepancy | Review in the settlement screen |
| `period_tie_out_difference` | Collections vs ledger revenue differ | Explain or correct; **blocks close** |
| `ledger_event_pending` / `ledger_event_dead_letter` | Outbox posting waiting / failed | Dead-lettered: investigate the `last_error`, fix, requeue |
| `payment_unposted` | Gateway payment has no journal | Repair, section 6.1 |
| `payment_posting_orphan` | Journal exists with no payment | Repair, section 6.2 |
| `refund_posting_gap` | Journaled refund total differs from refund | Repair, section 6.3 |

Categories: `timing` (5 days or younger, expected to clear on its own) vs `investigate`. Aging buckets: 0-30 current, 31-60 aging, 61-90 overdue, 90+ stale. Escalation defaults: manager at INR 10,000 or 60 days; owner at INR 50,000, 90 days, or any money-integrity item. Tune the thresholds in `ledger_reconciling_items` via a new migration, never by editing 044.

Cash and manual payments with no journal are listed by `SELECT * FROM ledger_unposted_payments('<property_uuid>') WHERE matched_by <> 'cashfree';`. They are reported but not auto-escalated pending the product decision in ADR-016 finding F4.

## 3. Statements

Bounds are half-open `[from, to)`; use the first instant of the next month as `to`.

```sql
-- Income statement for September 2026
SELECT * FROM ledger_income_statement('<property_uuid>', '2026-09-01+00', '2026-10-01+00');
-- Balance sheet as at the end of September (cumulative)
SELECT * FROM ledger_balance_sheet('<property_uuid>', '2026-10-01+00');
-- Cash flow (cash + bank), direct method
SELECT * FROM ledger_cash_flow('<property_uuid>', '2026-09-01+00', '2026-10-01+00');
-- Trial balance
SELECT * FROM ledger_trial_balance('<property_uuid>', '2026-10-01+00');
```

**Do not close unless all of these hold:**

1. Balance sheet row `check_difference` = 0 (assets = liabilities + equity + current earnings).
2. Cash-flow row `check_difference` = 0 and `closing_cash` equals the balance-sheet `bank` + `cash` balance.
3. Income statement `net_income` equals the movement in the balance-sheet `current_earnings` line for the same period.
4. No `unclassified` rows anywhere (a new account was added without updating `ledger_account_class`).
5. `ledger_reconciling_items` shows no `owner` escalation that is unexplained.

These are management statements on a collection basis, not statutory financial statements. Statutory accounts and tax filings come from your accountant.

## 4. Close

`POST /api/owner/finance/tie-out/close?period=YYYY-MM`

- Fails with `finance.periodNotCloseable` if the tie-out difference is non-zero **or the period has not ended**.
- Idempotent: closing an already closed period returns it unchanged.
- After close, journal lines dated in that month are rejected (`finance.periodClosed`) and `GET /api/owner/finance/tie-out?period=...` returns the frozen snapshot.

Retain evidence: export the four statements above to CSV (`\copy (SELECT ...) TO 'close_2026-09_income.csv' CSV HEADER`) and keep them with the reconciling-items output.

## 5. Late items after close

Post the correction in the **current open period** with a note referencing the closed month (the ordinary treatment in accounting systems). If the item truly cannot be handled that way, use section 7.

## 6. Repairs

All repairs are new entries; the journal is append-only. Test every repair on staging first. Each of these shapes is exercised by `scripts/sql/ledger_controls_test.sql`.

### 6.1 Payment committed, journal missing (`payment_unposted`)

Confirm the payment, its due kind, and that it truly has no journal, then post it with its **own id** as the source (idempotent: running twice fails on the unique index instead of double-posting):

```sql
\set pid  '<payment_uuid>'
\set prop '<property_uuid>'
BEGIN;
INSERT INTO financial_journal_entries (property_id, account_code, debit_paise, source_type, source_id, line_kind, occurred_at)
  SELECT :'prop', 'gateway_clearing', p.amount, 'payment', p.id, 'cash_in', now() FROM payments p WHERE p.id = :'pid';
INSERT INTO financial_journal_entries (property_id, account_code, credit_paise, source_type, source_id, line_kind, occurred_at)
  SELECT :'prop', 'rent_revenue', p.amount, 'payment', p.id, 'rent_collected', now() FROM payments p WHERE p.id = :'pid';
COMMIT;
```

Use `deposit_liability` / `utility_recovery_revenue` instead of `rent_revenue` if the due is a deposit or utility. For an unapplied payment use source type `unapplied_payment` and credit `unapplied_receipts`.

### 6.2 Journal exists, payment does not (`payment_posting_orphan`)

First confirm the payment really does not exist (not just a replication lag), then reverse it:

```sql
\set src '<source_uuid>'
BEGIN;
INSERT INTO financial_journal_entries (property_id, account_code, debit_paise, credit_paise, source_type, source_id, line_kind, occurred_at)
  SELECT property_id, account_code, credit_paise, debit_paise, 'correction', source_id, 'reversal_' || line_kind, now()
    FROM financial_journal_entries WHERE source_type = '<orig_type>' AND source_id = :'src';
COMMIT;
```

The reversal uses source type `correction` with the **same source id**; the detective control treats that as resolved.

### 6.3 Refund journaled for less than the refund (`refund_posting_gap`)

Post the missing allocation under the same refund id with **distinct line kinds** (the unique index forbids reusing them; this is why the original was dropped, ADR-016 F2). Use the account that matches the due kind (`rent_revenue`, `utility_recovery_revenue` or `deposit_liability`):

```sql
BEGIN;
INSERT INTO financial_journal_entries (property_id, account_code, debit_paise, source_type, source_id, line_kind, occurred_at)
  VALUES ('<prop>', 'rent_revenue', <gap_paise>, 'refund', '<refund_uuid>', 'refund_reversal_dr_alloc2', now());
INSERT INTO financial_journal_entries (property_id, account_code, credit_paise, source_type, source_id, line_kind, occurred_at)
  VALUES ('<prop>', 'gateway_clearing', <gap_paise>, 'refund', '<refund_uuid>', 'gateway_clearing_cr_alloc2', now());
COMMIT;
```

### 6.4 `LEDGER GAP` appears in the logs

That log line means a payment or refund committed without its journal. Run section 2, find the `payment_unposted` or `refund_posting_gap` row, and repair with 6.1 or 6.3. Alert on the log line **and** schedule `ledger_reconciling_items` so a missed log does not hide a gap.

## 7. Reopening a closed period (audited override)

Use only when a period genuinely must change. Always connect directly or through the app role in a **single transaction** and use `SET LOCAL` (never plain `SET`, which leaks across pooled connections).

```sql
BEGIN;
SET LOCAL app.actor = '<your name>: <ticket/reason>';   -- recorded in the immutable audit log
SET LOCAL app.reopen_period = 'on';
UPDATE period_tie_outs SET status = 'open', closed_at = NULL
 WHERE property_id = '<property_uuid>' AND period_month = '2026-08';
COMMIT;
-- make the correction, re-run section 3, then re-close (section 4)
```

Review every override:

```sql
SELECT occurred_at, event_type, property_id, period_month, actor, detail
  FROM ledger_control_overrides ORDER BY occurred_at DESC;
```

Every use is a control event; the owner (or the independent reviewer, once one exists) must review the log at each close. The audit table cannot be updated or deleted, even with the maintenance flag. `app.ledger_maintenance` (DELETE of journal rows or closed tie-outs) exists for disaster recovery only and is logged identically.

## 8. First deployment of 044

1. `psql "$DATABASE_URL" -f scripts/sql/ledger_preflight_audit.sql`: review every violation with the Finance Domain Owner. The triggers never rewrite history, so historical violations remain until corrected with new entries.
2. Apply migrations with `cmd/migrate` (one transaction per file; a failure rolls back that file).
3. On staging only: `psql "$STAGING_URL" -v ON_ERROR_STOP=1 -f scripts/sql/ledger_controls_test.sql` (expects `ALL 65 LEDGER CONTROL CHECKS PASSED`).
4. Re-run the pre-flight audit; expect only the violations you already accepted.
