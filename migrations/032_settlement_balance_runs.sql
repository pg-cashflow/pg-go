-- Migration 032: daily_settlement_balance_runs for immutable audit trail of all balancer runs
CREATE TABLE IF NOT EXISTS daily_settlement_balance_runs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    recon_date DATE NOT NULL,
    run_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    gateway_gross_paise BIGINT NOT NULL DEFAULT 0,
    gateway_net_settled_paise BIGINT NOT NULL DEFAULT 0,
    gateway_fees_paise BIGINT NOT NULL DEFAULT 0,
    gateway_tax_paise BIGINT NOT NULL DEFAULT 0,
    gateway_adjustment_paise BIGINT NOT NULL DEFAULT 0,
    gateway_in_transit_paise BIGINT NOT NULL DEFAULT 0,
    bank_credits_paise BIGINT NOT NULL DEFAULT 0,
    bank_debits_paise BIGINT NOT NULL DEFAULT 0,
    unapplied_quarantine_paise BIGINT NOT NULL DEFAULT 0,
    ledger_bank_dr_paise BIGINT NOT NULL DEFAULT 0,
    ledger_bank_cr_paise BIGINT NOT NULL DEFAULT 0,
    is_balanced BOOLEAN NOT NULL DEFAULT FALSE,
    discrepancy_paise BIGINT NOT NULL DEFAULT 0,
    discrepancies JSONB NOT NULL DEFAULT '[]'::jsonb,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS idx_balance_runs_prop_date
    ON daily_settlement_balance_runs (property_id, recon_date, run_at DESC);
