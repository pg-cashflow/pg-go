# Ticket 20: Track R.4 — Gate 04: End-to-End Money Lifecycle & Double-Entry Conservation

- **Type**: `wayfinder:task`
- **Status**: Open
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Specification**: [Test Readiness Specification](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/TEST_READINESS_SPECIFICATION.md)
- **Dependencies**: [Gate 01](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r01_gate01_skip_audit.md), [Gate 03](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/tickets/track_r03_gate03_api_contract_tests.md)
- **Tier**: PR Gate (< 10 min)

## Objective

Execute a full end-to-end money lifecycle test through real HTTP and real PostgreSQL.
Prove that double-entry journals balance at every lifecycle stage:
$$\sum\text{Debits} == \sum\text{Credits}$$
Prove zero integer-paise drift across the entire transaction path.

## Lifecycle Sequence

1. Tenant registers via join request.
2. Owner approves tenant and assigns room.
3. System generates rent due.
4. Tenant initiates checkout and creates payment intent.
5. Gateway webhook signals payment success.
6. System marks due paid and posts balanced ledger entries.
7. Tenant departs with partial refund calculation.
8. System posts departure settlement mirror journal.

## Karpathy Test Discipline

1. **Look at data first:**
   Run the lifecycle manually or via the Postman collection.
   Query `journal_lines` and `journal_entries` directly in PostgreSQL.
   Inspect debits, credits, and account IDs by hand.

2. **Check state at initialization:**
   Verify the test database starts clean.
   Assert the initial sum of all debits and credits is zero.

3. **Overfit one example:**
   Build one minimal scenario: 1 property, 1 room, 1 tenant, 1 rent due of ₹15,000 (1500000 paise).
   Run the 8-step lifecycle sequentially.
   Ensure every step passes and the ledger remains balanced.

4. **Compare with dumb baseline:**
   Execute raw SQL queries to sum all ledger lines:
   `SELECT SUM(debit_paise) - SUM(credit_paise) FROM journal_lines;`
   Assert the difference is exactly zero.

5. **Fix seeds:**
   Use fixed tenant IDs, room numbers, and transaction reference numbers.

6. **Change one thing at a time:**
   Verify join and due creation first.
   Then verify payment intent and webhook posting.
   Finally verify departure and refund posting.

## STE-100 Implementation Steps

1. Create test file `internal/api/e2e_money_lifecycle_test.go`.
2. Spin up an `httptest.Server` wrapping the router from `NewRouter`.
3. Connect to a real PostgreSQL test database.
4. Execute Step 1: Send `POST /api/join-requests` to create a join request.
5. Execute Step 2: Send `POST /api/owner/join-requests/:id/approve` to activate the tenant.
6. Execute Step 3: Call `billing.GenerateCycleDues` or trigger due creation.
7. Execute Step 4: Send `POST /api/tenant/dues/:id/intent` to create a payment intent.
8. Execute Step 5: Send signed `POST /api/pay/webhook/cashfree` using a valid HMAC signature.
9. Assert the due status changes to `paid`.
10. Query `journal_lines` for the payment transaction.
11. Assert total debits equal total credits.
12. Execute Step 6: Send `POST /api/owner/departures/:id/settle` for tenant departure.
13. Query `journal_lines` for the settlement transaction.
14. Assert the final global query `SELECT SUM(debit_paise) - SUM(credit_paise) FROM journal_lines` equals 0.

## Acceptance Criteria

- The end-to-end test executes in under 5 seconds against real PostgreSQL.
- Every intermediate journal entry has equal debits and credits.
- Final database query shows zero ledger imbalance.
- Zero floating-point arithmetic is used in the test.
