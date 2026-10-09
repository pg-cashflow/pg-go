# Ticket 21: Track R.5 — Gate 05: Gateway Webhook Replay, Out-of-Order & Concurrency

- **Type**: `wayfinder:task`
- **Status**: Open
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Specification**: [Test Readiness Specification](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/TEST_READINESS_SPECIFICATION.md)
- **Dependencies**: [Gate 04](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r04_gate04_e2e_money_flow.md)
- **Tier**: PR Gate (< 10 min)

## Objective

Validate webhook handling resilience under out-of-order delivery, duplicate replay, and concurrent processing.
Assert zero double-credits to dues and zero duplicate ledger postings.
Verify safe handling under the Go race detector (`go test -race`).

## Technical Context

Payment gateways often deliver webhooks out of order or multiple times.
A `PAYMENT_SUCCESS_WEBHOOK` may arrive after a refund webhook, or arrive five times simultaneously.
The system uses the `webhook_events` table for deduplication and row locks on dues (`SELECT ... FOR UPDATE`).
This mechanism must resist concurrent requests without deadlocks or double-crediting.

## Karpathy Test Discipline

1. **Look at data first:**
   Inspect standard Cashfree webhook payloads in `internal/cashfree/webhook.go`.
   Inspect rows inserted into `webhook_events` upon duplicate delivery.

2. **Check state at initialization:**
   Verify `webhook_events` is empty before each test scenario.
   Assert the target due is in `unpaid` status with zero payments.

3. **Overfit one example:**
   Send two identical webhooks sequentially.
   Assert the first returns HTTP 200 and marks the due paid.
   Assert the second returns HTTP 200 and does not create a second payment record.

4. **Compare with dumb baseline:**
   Count total payment rows for the due: `SELECT COUNT(*) FROM payments WHERE due_id = $1`.
   Assert the count is exactly 1.

5. **Fix seeds:**
   Use fixed event IDs (`evt_1001`) and order IDs (`order_due_1001`).

6. **Change one thing at a time:**
   Test sequential duplicates first.
   Then test 10 concurrent duplicate goroutines.
   Finally test out-of-order success followed by refund webhooks.

## STE-100 Implementation Steps

1. Create test file `internal/api/webhook_concurrency_test.go`.
2. Configure a local fake Cashfree gateway server to generate signed webhooks.
3. Test 1 (Duplicate Replay):
   - Send the same `PAYMENT_SUCCESS_WEBHOOK` three times sequentially.
   - Assert all three calls return HTTP 200 OK.
   - Assert exactly one payment record exists in PostgreSQL.
4. Test 2 (Concurrent Webhook Stampede):
   - Launch 10 goroutines sending the exact same webhook payload simultaneously.
   - Run under `go test -race`.
   - Assert zero race detector warnings.
   - Assert exactly one goroutine executes the payment mutation while nine hit the idempotency short-circuit.
5. Test 3 (Out-of-Order Webhooks):
   - Send a refund webhook before the payment success webhook.
   - Verify the system records the refund event safely without corrupting due state.
6. Check deadlocks:
   - Verify `pg_stat_database.deadlocks` does not increment during the test.

## Acceptance Criteria

- 10 concurrent duplicate webhooks result in exactly one payment record and one journal entry.
- All webhook requests return HTTP 200 OK within 2 seconds.
- Test runs clean under `go test -race` with zero data races.
- Zero deadlocks occur in PostgreSQL during concurrent execution.
