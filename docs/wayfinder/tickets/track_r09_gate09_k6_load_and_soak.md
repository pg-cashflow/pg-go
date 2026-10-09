# Ticket 25: Track R.9 — Gate 09: k6 Open-Model Load, Soak & Post-Run SQL Invariant Gate

- **Type**: `wayfinder:task`
- **Status**: Resolved
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Resolved By**: Track R.9 Stress Test Suite (fake gateway, k6 scenarios, and SQL invariants)
- **Specification**: [Test Readiness Specification](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/TEST_READINESS_SPECIFICATION.md)
- **Dependencies**: [Gate 04](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r04_gate04_e2e_money_flow.md), [Gate 05](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r05_gate05_webhook_replay_ordering.md)
- **Tier**: Nightly + Weekly Pre-Release

## Objective

Validate system performance, concurrency safety, and ledger invariants under load using Grafana k6.
Test at 10–20× peak expected traffic (50–100 requests per second).
Run a 60-minute soak test to detect memory leaks, connection pool starvation, and lock waits.
Enforce post-run SQL invariant verification: a load test with fast HTTP responses but a corrupted ledger is a failed test.

## Key Rules

1. **Stub the gateway**: Never call the live Cashfree API during load testing. Use a local fake gateway returning canned responses with controlled latency and failures.
2. **Target high-risk concurrency**: Send duplicate and out-of-order webhooks. Send concurrent checkouts targeting the same due. Send refunds racing with payments.
3. **Execute SQL invariant check after run**: Assert debits equal credits. Assert no payment is duplicated. Assert no due is over-applied.
4. **Test production connection pool**: Configure `DATABASE_MAX_CONNS=25` to match production PgBouncer and Neon settings.
5. **Soak execution**: Run for 30–60 minutes to uncover resource leaks.

## Performance Thresholds in k6

```javascript
thresholds: {
  'http_req_failed': ['rate<0.01'],                                   // Under 1% failure rate
  'http_req_duration{scenario:login_dashboard}': ['p(95)<400'],        // p95 < 400ms
  'http_req_duration{scenario:checkout_webhook}': ['p(95)<800'],       // p95 < 800ms
  'webhook_processing_duration': ['p(99)<2000'],                      // p99 < 2000ms
}
```

## Karpathy Test Discipline

1. **Look at data first:**
   Inspect the initial database state.
   Seed 50 properties, 500 tenants, and 500 unpaid dues.
   Verify initial ledger journal lines: `SELECT SUM(debit_paise) - SUM(credit_paise) FROM journal_lines;` must equal 0.

2. **Check state at initialization:**
   Verify connection pool metrics: `pg_stat_activity` connections must be below `DATABASE_MAX_CONNS` (25).
   Start the local fake Cashfree mock server.

3. **Overfit one example:**
   Execute a 10-second k6 run with 5 virtual users targeting checkout and webhook settlement.
   Verify all requests return HTTP 200 and the ledger stays balanced.

4. **Compare with dumb baseline:**
   Compare expected paid dues count with actual paid dues count:
   `SELECT COUNT(*) FROM dues WHERE status = 'paid';`
   Assert it equals the number of unique successful webhook events.

5. **Fix seeds:**
   Use fixed tenant IDs and deterministic order IDs in the k6 script.

6. **Change one thing at a time:**
   Run Scenario 1 (read mix) first.
   Run Scenario 2 (checkout + webhook concurrency) second.
   Run Scenario 3 (60-minute soak test) third.

## STE-100 Implementation Steps

1. Expand `scripts/loadtest/` with dedicated k6 test files:
   - `scripts/loadtest/scenarios/read_dashboard.js`
   - `scripts/loadtest/scenarios/checkout_webhook.js`
   - `scripts/loadtest/soak_test.js`
   - `scripts/loadtest/fake_gateway.go`
   - `scripts/loadtest/post_run_invariants.sql`
2. Configure the fake gateway server:
   - Listen on local port 8081.
   - Return instant 200 responses for payment intent creations.
   - Dispatch HMAC-signed webhooks to `/api/pay/webhook/cashfree` with configurable latency.
3. Write `checkout_webhook.js` using `ramping-arrival-rate`:
   - Ramp traffic from 10 RPS to 60 RPS over 5 minutes.
   - Hold at 60 RPS for 10 minutes.
   - Generate concurrent checkout attempts on shared dues.
4. Write `post_run_invariants.sql`:
   - Check 1: `SELECT SUM(debit_paise) - SUM(credit_paise) FROM journal_lines;` (Must equal 0).
   - Check 2: Check for any duplicate payments: `SELECT normalized_utr, COUNT(*) FROM payments GROUP BY normalized_utr HAVING COUNT(*) > 1;` (Must return 0 rows).
   - Check 3: Check for overpaid dues: `SELECT id FROM dues WHERE paid_amount_paise > amount_paise;` (Must return 0 rows).
   - Check 4: Check for deadlocks: `SELECT deadlocks FROM pg_stat_database WHERE datname = current_database();` (Must be 0).
5. Build the CI runner script `scripts/loadtest/run.sh`:
   - Start test database and fake gateway.
   - Start `server` binary with `DATABASE_MAX_CONNS=25`.
   - Run k6 test suite.
   - Execute `post_run_invariants.sql`. If any query fails, exit with error code 1.

## Acceptance Criteria

- k6 executes with open-model arrival rates at 60 RPS without exceeding the 1% error threshold.
- Checkout p95 response time is under 800 ms.
- Post-run SQL check proves exactly zero paise imbalance across all journals.
- Zero duplicate payments are recorded for duplicate webhook deliveries.
- 60-minute soak run completes without memory leakage or database connection exhaustion.

## Resolution

1. **Fake Gateway Harness** ([fake_gateway.go](file:///c:/Users/divak/Downloads/pg-go/scripts/loadtest/fake_gateway.go)):
   - High-throughput mock Cashfree PG server with zero external network dependencies.
   - Computes standard `Base64(HMAC-SHA256(timestamp + rawBody, secret))` signatures.
   - Emits asynchronous, concurrent duplicate webhooks for stampede and idempotency testing.
2. **K6 Stress Scenarios**:
   - [read_dashboard.js](file:///c:/Users/divak/Downloads/pg-go/scripts/loadtest/scenarios/read_dashboard.js): Multi-tenant read load across `/api/owner/dashboard/summary`, `/api/owner/dues`, and `/api/search` (up to 100 VUs).
   - [checkout_webhook.js](file:///c:/Users/divak/Downloads/pg-go/scripts/loadtest/scenarios/checkout_webhook.js): Open-model `ramping-arrival-rate` targeting 60 RPS with 30% intentional duplicate webhook stampedes.
   - [soak_test.js](file:///c:/Users/divak/Downloads/pg-go/scripts/loadtest/soak_test.js): Extended soak scenario testing resource exhaustion under `DATABASE_MAX_CONNS=25`.
3. **Post-Run SQL Invariants** ([post_run_invariants.sql](file:///c:/Users/divak/Downloads/pg-go/scripts/loadtest/post_run_invariants.sql)):
   - Verifies 6 critical database invariants: total debit/credit conservation, per-source balanced entries, zero duplicate provider payment IDs, zero duplicate UTRs, zero overpaid dues, and zero database deadlocks.
4. **Automated Karpathy 6-Step Runners**:
   - [run.ps1](file:///c:/Users/divak/Downloads/pg-go/scripts/loadtest/run.ps1) (Windows PowerShell) and [run.sh](file:///c:/Users/divak/Downloads/pg-go/scripts/loadtest/run.sh) (Linux/CI).

