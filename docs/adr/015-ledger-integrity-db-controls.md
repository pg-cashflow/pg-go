# ADR-015: Database-Enforced Ledger Integrity Controls

**Status:** Accepted  
**Date:** 2026-10-03  
**Deciders:** Engineering Lead, Finance Domain Owner  
**Migration:** `043_ledger_integrity_controls.sql`

---

## Context

The `financial_journal_entries` table is the authoritative double-entry ledger for every property. At the time this ADR is written:

- No database constraint prevents a row from being `UPDATE`d or `DELETE`d; the immutability rule is enforced only at the application layer.
- No database constraint verifies that every `(source_type, source_id)` group is balanced; a half-written transaction (e.g. application crash mid-batch, or a silently-discarded error) leaves the ledger in an inconsistent state.
- `period_tie_outs` rows with `status = 'closed'` can be overwritten or deleted; a closed-period invariant exists only as a convention.

One known instance of the silent-discard problem is in `handlers_pay_webhooks.go:295`:

```go
_ = h.Finance.MirrorPayment(ctx, p, due0)
```

The error is discarded with `_ =`.  If `MirrorPayment` fails (e.g. transient DB error, `MakeLines` length mismatch), the payment is marked as settled in the gateway tables but the journal carries no entry.  The C-2 deferred balance trigger does **not** catch this case (the `INSERT` never happens), but it will catch any future bug where only one of two lines is inserted within the same transaction.

---

## Decision

Enforce three invariants at the **PostgreSQL trigger layer**, independent of application code:

### C-1 — Append-Only Journal (`trg_journal_append_only`)

| Operation | Behaviour |
|-----------|-----------|
| `UPDATE`  | Always rejected. Callers must post a reversing entry. |
| `DELETE`  | Rejected unless `SET LOCAL app.ledger_maintenance = 'on'` is active in the session. |

Trigger type: `BEFORE UPDATE OR DELETE`, `FOR EACH ROW`.

### C-2 — Double-Entry Balance (`trg_journal_balanced`)

After every `INSERT`, at **commit time** (deferred), the trigger re-reads the running totals for the inserted row's `(source_type, source_id)` and asserts `SUM(debit_paise) = SUM(credit_paise)`.

| Mechanism | Detail |
|-----------|--------|
| Trigger type | `CONSTRAINT TRIGGER … AFTER INSERT … DEFERRABLE INITIALLY DEFERRED` |
| Scope | Per `(source_type, source_id)` — one payment, expense, departure settlement, etc. |
| False-positive risk | None: all `Mirror*` helpers insert the full set of balanced lines in a single transaction. |

> **Important:** this trigger does **not** protect against the `_ = h.Finance.MirrorPayment(...)` call-site where the error is swallowed before the `INSERT` even reaches the DB.  That is a separate Go-layer issue tracked in the audit report.  The trigger catches the subset of bugs where lines are inserted but unbalanced.

### C-3 — Frozen Closed Periods (`trg_tieout_closed_frozen`)

| Operation | `status` | Behaviour |
|-----------|----------|-----------|
| `UPDATE`  | `'closed'` | Rejected unless `SET LOCAL app.reopen_period = 'on'`. |
| `DELETE`  | `'closed'` | Rejected unless `SET LOCAL app.ledger_maintenance = 'on'`. |
| Any op    | `'open'`   | Passes through unchanged. |

Trigger type: `BEFORE UPDATE OR DELETE`, `FOR EACH ROW`.

---

## Escape Hatches

Both GUCs are checked with `current_setting(..., true)` (the `missing_ok` overload), so they default to `'off'` in sessions that never set them.

```sql
-- Allow deletes on the journal / deleting a closed tie-out (tests, emergency data surgery):
SET LOCAL app.ledger_maintenance = 'on';

-- Allow updating (reopening) a closed period tie-out:
SET LOCAL app.reopen_period = 'on';
```

`SET LOCAL` scope is restricted to the current transaction; the GUC resets automatically on `COMMIT` or `ROLLBACK`.  Neither GUC should appear in application code paths.

---

## Consequences

### Positive

- Any application bug that produces an unbalanced write for a given source is caught at the DB level on commit, before the transaction is visible to readers.
- Journal rows cannot be silently corrupted by ORM-level `save()` calls or ad-hoc SQL patches.
- Closed periods are write-protected without requiring application-layer guards in every query path.

### Negative / Trade-offs

- **Test setup cost:** integration tests that insert partial journal rows for setup purposes must either use `SET LOCAL app.ledger_maintenance = 'on'` or insert balanced sets.  Existing `invariants_test.go` tests call `MirrorPayment` which always inserts balanced pairs, so they are unaffected.
- **C-2 performance:** the deferred balance check does one aggregate query per inserted row at commit time.  For batch-insert scenarios (e.g. bulk migration) this adds overhead; use `SET CONSTRAINTS trg_journal_balanced DEFERRED` (already the default) and ensure rows are inserted in balanced pairs within the same transaction.
- **No protection for the `_ = MirrorPayment(...)` silent discard:** C-2 catches imbalance within a transaction; it does not help when the application never reaches the `INSERT`.  That call site should be fixed separately to log and/or propagate the error.

---

## Alternatives Considered

| Option | Reason rejected |
|--------|-----------------|
| Application-layer-only enforcement | Already in place; already violated by the `_ =` call site. |
| CHECK constraint on debit == credit per row | The existing `chk_journal_one_side` constraint already enforces one side per row; balance must be verified across the pair of rows for the same source. |
| Postgres `GENERATED ALWAYS` column for running totals | Too complex; breaks the simple two-column `debit_paise / credit_paise` model. |
| Event-sourced ledger with immutable event log at app layer | Out of scope for current architecture; revisit in a future greenfield phase. |
