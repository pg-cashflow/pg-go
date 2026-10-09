# Ticket 24: Track R.8 — Gate 08: Process Crash, Network Fault & Poison Recovery

- **Type**: `wayfinder:task`
- **Status**: Open
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Specification**: [Test Readiness Specification](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/TEST_READINESS_SPECIFICATION.md)
- **Dependencies**: [Gate 04](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r04_gate04_e2e_money_flow.md)
- **Tier**: Nightly

## Objective

Validate system resilience and transactional integrity against process crashes, network cuts, and worker restarts.
Assert that killed transactions roll back completely without partial state in PostgreSQL.
Assert that `LedgerOutboxWorker` and background cron jobs resume safely after unexpected termination.

## Technical Context

PG Cashflow uses single-database transactions with row-level locks for all financial mutations.
If a server crashes while processing `SettleDepartureUnderLock` or `HandleWebhook`, Postgres must roll back the transaction.
If the ledger outbox worker is killed while dispatching an unmirrored event, subsequent restarts must pick up the event using `SKIP LOCKED`.

## Karpathy Test Discipline

1. **Look at data first:**
   Inspect `ledger_outbox_events` table before and after crash simulations.
   Inspect partial records or lock states using `pg_locks` and `pg_stat_activity`.

2. **Check state at initialization:**
   Verify no unhandled outbox events or lingering locks exist before starting tests.

3. **Overfit one example:**
   Begin a departure settlement transaction.
   Insert partial deduction rows.
   Simulate a client process kill (`SIGKILL`) before `tx.Commit()`.
   Verify the deduction rows do not persist in PostgreSQL.

4. **Compare with dumb baseline:**
   Query target tables before and after the simulated crash:
   `SELECT COUNT(*) FROM deductions WHERE departure_id = $1`.
   Assert the count matches the pre-crash baseline (0).

5. **Fix seeds:**
   Deterministic process IDs and synthetic fault sequences.

6. **Change one thing at a time:**
   Test database transaction crash first.
   Test outbox worker recovery second.
   Test network latency injection third.

## STE-100 Implementation Steps

1. Create test file `internal/finance/crash_recovery_test.go`.
2. Test 1 (Transaction Atomicity on Sudden Process Abort):
   - Open a database connection.
   - Start transaction `tx.Begin(ctx)`.
   - Insert financial journal lines.
   - Terminate the database client connection violently without sending `COMMIT` or `ROLLBACK`.
   - Open a new connection.
   - Assert zero uncommitted journal lines exist in the database.
3. Test 2 (Outbox Worker Recovery after Mid-Batch Kill):
   - Enqueue 5 `ledger_outbox_events`.
   - Start `LedgerOutboxWorker`.
   - Kill the worker process after processing 2 events.
   - Start a fresh `LedgerOutboxWorker`.
   - Assert the remaining 3 events are processed successfully.
   - Assert no event is duplicated or skipped.
4. Test 3 (Network Partition with Toxiproxy):
   - Route database traffic through a Toxiproxy proxy.
   - Cut the TCP connection during a payment webhook request.
   - Re-enable the connection.
   - Retry the webhook.
   - Assert the system recovers and settles the payment cleanly.

## Acceptance Criteria

- Zero orphaned or partial records persist after simulated crash.
- `LedgerOutboxWorker` resumes from the last uncommitted event without data loss.
- System recovers gracefully from simulated 5-second network cuts.
- Tests execute automatically in the nightly CI workflow.
