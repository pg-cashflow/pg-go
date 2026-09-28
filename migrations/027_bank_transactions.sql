-- Migration 027: Bank Statement Ingress & Unidentified Deposit Reconciliation (Track O / Ticket 14)

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
