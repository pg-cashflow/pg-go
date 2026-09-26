# Ticket 4: Departure Settlement Mirror Post-Commit Resilience

- **Type**: `wayfinder:task`
- **Status**: Open (Claimed next)
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)

## Objective

In `internal/postgres/payout_repo.go` (`SettleDepartureUnderLock`), the departure transaction commits before posting the mirror financial journal entry to the double-entry ledger. If the process crashes or encounters network partition between the PostgreSQL transaction commit and `MirrorDepartureSettlement`, the departure record remains settled in the operational DB but the accounting ledger fails to reflect the settlement.

Audit and design resilience for this post-commit mirror write:
1. Evaluate transactional outbox pattern via `outbox_events` table vs asynchronous reconciliation job.
2. Ensure at-least-once delivery with idempotent ledger posting (`idempotency_key` or event deduplication).
3. Verify zero discrepancy in double-entry books.
