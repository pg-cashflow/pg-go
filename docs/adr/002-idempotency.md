# ADR 002: Idempotency Keys on All Financial Mutations

## Status
Accepted

## Context
Financial mutations (capital infusions, expense recording, split vendor payments, manager out-of-pocket advances, and owner reimbursements) can be retried across unreliable network conditions or concurrent client actions. Duplicate postings would corrupt the financial ledger, misstate liability payables, and distort ROI metrics.

## Decisions

1. **Mandatory Idempotency Keys**:
   - Every financial mutation endpoint requires an `Idempotency-Key` HTTP header.
   - Missing key results in `400 Bad Request` (`ErrIdempotencyRequired`).

2. **Database-Enforced Uniqueness**:
   - Uniqueness is enforced at the database level scoped by `property_id`:
     - `capital_transactions (property_id, idempotency_key)`
     - `expenses (property_id, idempotency_key)`
     - `expense_payments (property_id, idempotency_key)`
     - `manager_advances (property_id, idempotency_key)`
     - `manager_reimbursements (property_id, idempotency_key)`
   - Duplicate attempts fail with `23505 unique_violation`, which maps directly to `409 Conflict` (`ErrDuplicateIdempotency`), matching the platform's UTR duplicate detection pattern.

3. **Double-Entry Journal Mirror Idempotency**:
   - Journal lines mirror incoming events idempotently via composite uniqueness:
     - `financial_journal_entries (source_type, source_id, line_kind)`
   - Retried payment settlements or reward redemptions cannot double-post ledger entries.
