-- Migration 028: Bank Account Profiles & Ingress Multi-Account Grounding

CREATE TABLE IF NOT EXISTS bank_accounts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    bank_name VARCHAR(100) NOT NULL,
    account_type VARCHAR(20) NOT NULL DEFAULT 'savings' CHECK (account_type IN ('savings', 'current')),
    account_number_last4 VARCHAR(4) NOT NULL,
    label VARCHAR(100) NOT NULL DEFAULT '',
    statement_profile VARCHAR(50) NOT NULL DEFAULT 'generic',
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_bank_accounts_property
    ON bank_accounts (property_id, is_active);

-- Link bank_transactions to bank_accounts and add classification/metadata
ALTER TABLE bank_transactions
    ADD COLUMN IF NOT EXISTS bank_account_id UUID REFERENCES bank_accounts(id) ON DELETE CASCADE,
    ADD COLUMN IF NOT EXISTS is_reversal BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS is_internal_transfer BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS payer_phone VARCHAR(20),
    ADD COLUMN IF NOT EXISTS payer_vpa VARCHAR(100),
    ADD COLUMN IF NOT EXISTS classification VARCHAR(30) NOT NULL DEFAULT 'unclassified'
        CHECK (classification IN ('unclassified', 'rent', 'interest_income', 'owner_equity', 'non_pg_income', 'reversal', 'internal_transfer'));

CREATE INDEX IF NOT EXISTS idx_bank_transactions_account
    ON bank_transactions (bank_account_id);

CREATE INDEX IF NOT EXISTS idx_bank_transactions_classification
    ON bank_transactions (property_id, classification);
