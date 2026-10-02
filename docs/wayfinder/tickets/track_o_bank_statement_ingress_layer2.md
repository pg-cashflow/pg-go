# Ticket 14: Track O — Stream 3 Layer 2: Bank Statement Ingress & Unidentified Deposit Reconciliation

- **Type**: `wayfinder:task`
- **Status**: Resolved (Fully Implemented & Verified)
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Prerequisite**: Track N (Gateway Settlement Ingress & Reconciliation) resolved.

---

## 1. Objective

Implement **Stream 3 Layer 2 (Bank Statement Ingress & Unidentified Deposit Reconciliation)**:
1. **Fix Silent Cross-Attribution Bug**: Prevent heuristic `AmountDateWindowMatcher` from auto-settling dues. Disentangle Tier 1 (Deterministic Auto-Post on `PG-XXXXXX` due code) from Tier 2 (Heuristic Staging for owner confirmation).
2. **Ingest Bank Statement Lines**: Support manual CSV statement upload (`POST /api/owner/bank-statements/import-csv`) using the canonical alias-tolerant parser in `internal/csv/parser.go`.
3. **Double-Entry Quarantine Invariant**:
   - For every credit row cleared in the bank statement, cash in the bank MUST be recognized immediately:
     - **Tier 1 (Deterministic)**: Dr `bank`, Cr `rent_revenue` (or `deposit_liability`). Due marked paid.
     - **Tier 2 (Heuristic Suggestion)** & **Unmatched**: Dr `bank`, Cr `unapplied_receipts`. Due remains pending.
     - **Owner Confirms Match**: Dr `unapplied_receipts`, Cr `rent_revenue`. Due marked paid.
     - **Owner Refunds Deposit**: Dr `unapplied_receipts`, Cr `bank`.
4. **Retire `ExpenseImportSuggestion`**: Supersede the half-built `SuggestCSVDebit` / `ExpenseImportSuggestion` path with a durable, idempotent `bank_transactions` table.
5. **Human-Gated Dual-Control / Step-Up**: Gating manual confirmation and refund endpoints via `verifyDualControlOrStepUp`.
6. **Track K Invariant Evals**: Continuous property-based invariant test asserting $\sum \text{Dr bank} == \sum \text{CSV Credits}$ with zero paise dropped.

---

## 2. Threat Model & Failure Modes

1. **Silent Rent Cross-Attribution**:
   - Two tenants on the same property with the same rent (e.g. ₹12,000) due around the 5th of the month.
   - Bank statement contains one direct transfer with generic narration `UPI/1234567890/Transfer`.
   - Previous behavior: `AmountDateWindowMatcher` matched whichever due candidate was returned first and closed it, leaving the actual payer's due open and charging late fees.
   - **Remediation**: Tier 2 matches are staged as `suggested_match` and quarantined to `unapplied_receipts`. They NEVER auto-settle.
2. **The "Unmatched Drop" Accounting Gap**:
   - Bank statement row doesn't match any due code or candidate amount.
   - Previous behavior: Handler called `failed++` and `SuggestCSVDebit`. The bank statement showed ₹50,000 cleared in the bank, but the double-entry journal recorded ₹0, corrupting the bank balance tie-out.
   - **Remediation**: Unmatched transactions immediately debit `bank` and credit `unapplied_receipts`.
3. **Duplicate Statement Upload**:
   - Owner uploads statement covering August 1–15, then uploads statement covering August 10–25.
   - Overlapping rows (August 10–15) must not double-credit `bank` or double-settle dues.
   - **Remediation**: Database unique constraint `(property_id, txn_id)` and transactional upsert / skip-existing.

---

## 3. Data Model & Migration (`027_bank_transactions.sql`)

```sql
CREATE TABLE IF NOT EXISTS bank_transactions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    txn_id TEXT NOT NULL DEFAULT '',
    amount_paise BIGINT NOT NULL CHECK (amount_paise > 0),
    row_type VARCHAR(10) NOT NULL DEFAULT 'credit' CHECK (row_type IN ('credit', 'debit')),
    txn_date DATE NOT NULL,
    narration TEXT NOT NULL DEFAULT '',
    closing_balance_paise BIGINT,                       -- Stored for statement reconciliation tie-out
    occurrence_index INT NOT NULL DEFAULT 1,            -- 1-based index within statement file for identical same-day rows
    dedup_hash TEXT NOT NULL,                           -- Composite hash: sha256(prop|date|amt|type|txnid|bal_or_occ)
    status VARCHAR(30) NOT NULL DEFAULT 'unmatched'
        CHECK (status IN ('matched', 'suggested_match', 'unmatched', 'refunded', 'ignored_debit')),
    matched_due_id UUID REFERENCES dues(id) ON DELETE SET NULL,
    suggested_due_id UUID REFERENCES dues(id) ON DELETE SET NULL,
    confidence_score NUMERIC(3,2) DEFAULT 0.00,
    matched_at TIMESTAMPTZ,
    matched_by UUID REFERENCES users(id) ON DELETE SET NULL,
    journal_entry_id UUID REFERENCES financial_journal_entries(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Unique composite index for idempotent statement deduplication
CREATE UNIQUE INDEX IF NOT EXISTS uq_bank_transactions_prop_dedup
    ON bank_transactions (property_id, dedup_hash);

CREATE INDEX IF NOT EXISTS idx_bank_transactions_prop_status
    ON bank_transactions (property_id, status);

CREATE INDEX IF NOT EXISTS idx_bank_transactions_prop_date
    ON bank_transactions (property_id, txn_date);

CREATE INDEX IF NOT EXISTS idx_bank_transactions_txn_id
    ON bank_transactions (txn_id) WHERE txn_id != '';
```

> [!NOTE]
> **Debit Tracking & Statement Tie-Out**: Debits (withdrawals) are ingested with `row_type = 'debit'` and `status = 'ignored_debit'` to preserve the audit trail and running `closing_balance_paise` without matching them to rent dues or double-counting them as receipts. Payout debits are posted via payout batches, and storing closing balances allows future bank reconciliation reports to surface non-payout cash outflows (bank fees, ATM, direct debits).


---

## 4. Architectural Invariants

1. **Universal Lock Hierarchy**:
   All operations that match or reallocate bank transactions acquire row locks in strict order:
   `tenants` $\rightarrow$ `tenant_streaks` $\rightarrow$ `dues` $\rightarrow$ `payments` $\rightarrow$ `bank_transactions`.
2. **Double-Entry Balance Conservation**:
   - Initial Ingestion:
     $$\text{Matched: } \text{Dr } \mathtt{bank} \quad / \quad \text{Cr } \mathtt{rent\_revenue}$$
     $$\text{Unmatched/Suggested: } \text{Dr } \mathtt{bank} \quad / \quad \text{Cr } \mathtt{unapplied\_receipts}$$
   - Subsequent Confirmation:
     $$\text{Dr } \mathtt{unapplied\_receipts} \quad / \quad \text{Cr } \mathtt{rent\_revenue}$$
   - Sum of debits strictly equals sum of credits with zero integer-paise drift.
3. **No Account Aggregator (AA) Scope Creep**:
   - Manual CSV upload only (`POST /api/owner/bank-statements/import-csv`).

---

## 5. Execution Plan

1. **Step 1 (Immediate Matcher Safety)**:
   - Separate `DueCodeMatcher` (Tier 1 deterministic) from `AmountDateWindowMatcher` in `internal/payment/matcher.go`.
   - Update `MatchPayment` to only auto-settle when matched by `DueCode` (or exact registered UTR).
   - Write isolated unit test proving same-amount, same-window dues do not auto-settle.
2. **Step 2 (Database & Domain)**:
   - Create migration `027_bank_transactions.sql`.
   - Define domain model `BankTransaction` in `internal/domain/finance.go`.
   - Implement `BankTransactionRepo` in `internal/postgres/bank_transaction_repo.go`.
3. **Step 3 (Financial Mirror & Ingestion Engine)**:
   - Add `MirrorBankCredit` and `MirrorUnappliedAllocation` in `internal/finance/mirror.go`.
   - Refactor `OwnerImportPaymentsCSV` in `internal/api/handlers_owner.go` to persist all rows into `bank_transactions` and apply the quarantine entry.
4. **Step 4 (Endpoints & Resolution Flow)**:
   - `GET /api/owner/bank-statements/transactions` (filterable by status: `suggested_match`, `unmatched`, `matched`).
   - `POST /api/owner/bank-statements/transactions/:id/confirm-match` (with `verifyDualControlOrStepUp`).
   - `POST /api/owner/bank-statements/transactions/:id/refund` (with `verifyDualControlOrStepUp`).
5. **Step 5 (Track K Invariant Evals & Verification)**:
   - Add property-based test in `internal/finance/invariants_test.go` asserting bank debit conservation across all imported rows.
