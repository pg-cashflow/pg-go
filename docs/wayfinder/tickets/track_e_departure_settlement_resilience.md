# Ticket 4: Departure Settlement Mirror Post-Commit Resilience

- **Type**: `wayfinder:task`
- **Status**: Resolved
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)

## Objective

In `internal/postgres/payout_repo.go` (`SettleDepartureUnderLock`), the departure transaction commits before posting the mirror financial journal entry to the double-entry ledger. If the process crashes or encounters network partition between the PostgreSQL transaction commit and `MirrorDepartureSettlement`, the departure record remains settled in the operational DB but the accounting ledger fails to reflect the settlement.

## Implementation Details

1. **Transactional Outbox Table**: Added `migrations/023_ledger_outbox_events.sql` creating dedicated `ledger_outbox_events` table with partial index for pending/retryable events. Dedicated table isolates financial journal durability from notification retry policies, avoiding silent swallowing by notification dispatchers.
2. **Atomic In-Transaction Enqueue**: In `internal/postgres/payout_repo.go` (`SettleDepartureUnderLock`), the `ledger_outbox_events` row (event type `departure_settlement_mirror`, payload with departure ID and balanced amounts, `idempotency_key = "departure_settlement:" + dep.ID`) is inserted within the same active PostgreSQL transaction before `tx.Commit(ctx)`.
3. **Inline Fast-Path & Background Processor**: Post-commit attempts an inline mirror write and immediately marks `processed_at = now()`. If the process crashes or network blips, `internal/finance/ledger_worker.go` (`LedgerOutboxWorker`) polls pending events with `FOR UPDATE SKIP LOCKED`, executes `MirrorDepartureSettlement`, and handles exponential backoff.
4. **Dead-Letter Loud Escalation**: If `attempt_count >= max_attempts`, the event is never silently dropped; it is marked `failed_at` and triggers a loud `slog.Error("CRITICAL: ledger outbox event exceeded max attempts - financial books desynchronized", ...)` alert.
5. **Secondary Reconciliation Safety Net**: Implemented `internal/finance/departure_reconciliation.go` (`ReconcileDepartureSettlements`), periodically comparing settled departures against `financial_journal_entries` older than an age threshold (e.g. 1 hour) and flagging un-mirrored departures without silent mutation.
6. **Tests**: Validated via `internal/finance/ledger_worker_test.go` (`TestLiveLedgerOutboxWorkerAndReconciliation`) covering transactional enqueue, worker dispatch, exponential backoff, max-attempts escalation, and reconciliation sweep.
