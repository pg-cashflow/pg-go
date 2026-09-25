# ADR 008: Gateway Clearing, Resolve-then-Lock & Forward-Only Reconciliation

## Status
Accepted
**Date:** 2026-09-24
**Decider:** Divakar (Solo Developer)

## Context
Payment gateways operate asynchronously via webhooks, customer redirect returns, and periodic polling. In flight, events may arrive out of order, duplicate, or race against manual owner settlements or admin refunds.

## Decisions

### 1. Universal Lock Hierarchy (Single Source of Truth)
To eliminate deadlocks across concurrent webhooks, pollers, owner refunds, cash settlements, and departure payouts, **every database transaction that touches money MUST acquire row locks in this exact hierarchical order**:
1. **`tenants`**: `SELECT id FROM tenants WHERE id = $1 FOR UPDATE`
2. **`tenant_streaks`** (locking points balance anchor): `SELECT points_balance FROM tenant_streaks WHERE tenant_id = $1 FOR UPDATE`
3. **`dues`**: `SELECT * FROM dues WHERE id = ANY($1) ORDER BY due_date ASC, id ASC FOR UPDATE`
4. **`payment_intents`**: `SELECT * FROM payment_intents WHERE id = $1 FOR UPDATE`
5. **`payment_intent_dues`**: `SELECT * FROM payment_intent_dues WHERE payment_intent_id = $1 ORDER BY allocation_order ASC, id ASC FOR UPDATE`
6. **`payments`**: `SELECT * FROM payments WHERE id = ANY($1) ORDER BY created_at ASC, id ASC FOR UPDATE`
7. **`gateway_refunds`**: `SELECT * FROM gateway_refunds WHERE id = ANY($1) ORDER BY created_at ASC, id ASC FOR UPDATE`
8. **`tenant_departures`**: `SELECT * FROM tenant_departures WHERE id = $1 FOR UPDATE`
9. **`departure_deductions`**: `SELECT * FROM departure_deductions WHERE departure_id = $1 FOR UPDATE`
*(No code path may acquire locks out of this order or lock child rows before parents).*

### 2. Resolve-then-Lock Pattern
- Webhook arrival begins with an **unlocked navigation read** to identify the candidate tenant, due, or payment.
- The transaction then acquires row locks strictly according to the **Universal Lock Hierarchy** defined in §1 above.
- Under lock, the entity state is **re-verified** as ground truth before applying mutations.

### 2. Gateway Clearing Account (`gateway_clearing`)
- Captured payments debit `gateway_clearing` (asset receivable from gateway) and credit `revenue` / `accounts_receivable`.
- Gateway settlements into the merchant bank account debit `bank` and credit `gateway_clearing`.
- Deductions for MDR / platform transaction fees debit `gateway_fees_expense`.

### 3. Forward-Only Status Transitions
- Dues status transitions are forward-only: `pending` $\rightarrow$ `partial` $\rightarrow$ `paid`. Dues may not regress unless reversed through an explicit, auditable refund allocation.
- Gateway refunds follow forward-only lifecycle: `initiated` $\rightarrow$ `pending` $\rightarrow$ `succeeded` / `failed` / `cancelled`. A terminal status (`succeeded`, `failed`, `cancelled`) is immutable.

### 4. Derived Due Status Invariance
- Due balances and statuses are strictly derived:
  $$\text{Net Paid} = \sum \text{payment\_allocations} - \sum \text{succeeded attributable refund\_allocations}$$
- Eliminates paise-drift and circular balance updates.

### 5. Multi-Due Junction Invariants & Status Trigger Synchronization
- **Database-Enforced Trigger-Only Mutation Guard**: `payment_intent_dues.status` is **never** written or updated directly by application code after initial creation. All state mutations (`superseded`, `paid`, `failed`, `expired`) cascade synchronously from `payment_intents.status` via the database trigger `sync_payment_intent_dues_status()`. This rule is **actively enforced at the database layer** by a `BEFORE UPDATE` guard trigger (`trg_prevent_direct_payment_intent_dues_status_update`) checking `pg_trigger_depth() <= 1`, which strictly raises an exception if a direct application query attempts to mutate `status` outside the trigger cascade.
- **Expected Supersession Boundary**: When a new checkout option is chosen, the supersession step invalidates the entire older intent. Dues that were bundled in the old multi-due intent but are omitted from the new discrete selection have their in-flight session closed and remain in their canonical `pending` status with zero money risk. They will receive a fresh session token when the tenant subsequently selects an option covering them.

## Consequences
- Complete deadlock immunity across concurrent webhook deliveries, checkout sessions, and background pollers.
- True double-entry ledger balancing with zero float drift.
- Guaranteed database-level prevention of concurrent active checkout sessions across both single-due and multi-due intents.
