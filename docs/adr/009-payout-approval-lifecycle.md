# ADR 009: Payout Approval Lifecycle & Batch Release

## Status
Accepted
**Date:** 2026-09-24
**Decider:** Divakar (Solo Developer)

## Context
Operating a PG involves regular operational disbursements: staff salaries, utility payments (electricity, water, internet), vendor maintenance, and deposit refunds. These payouts must have clear separation of preparation, approval, and release.

## Decisions

### 1. Multi-Stage Payout Lifecycle
- `payout_records` transitions through explicit states:
  `draft` $\rightarrow$ `pending_approval` $\rightarrow$ `approved` $\rightarrow$ `processing` $\rightarrow$ `completed` / `failed`.
- Drafts may be generated automatically by scheduled cron jobs (e.g. monthly payroll or deposit refund settlement sheets).
- Approval requires authenticated owner verification.

### 2. Line-Item Traceability (`payout_record_items`)
- Payout batches link to individual payments or payable dues via `payout_record_items`.
- Enforces `UNIQUE(payment_id)` and `UNIQUE(due_id)` where applicable to prevent double-settlement or dual reimbursement.

### 3. Execution Rails
- **Phase 1**: Owner downloads generated payout CSV / payment sheet, executes manual NEFT/IMPS transfers via bank portal, and enters the bank UTR to mark the batch `completed`.
- **Phase 2**: Automated direct bank transfer / gateway payouts adapter plugged in behind the same state lifecycle.

## Consequences
- Error prevention against duplicate payments to vendors and departing tenants.
- Clean operational audit trail for tax, expense tracking, and financial statements.
