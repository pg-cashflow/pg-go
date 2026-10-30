-- Migration 091: Add settlement mode and cash disbursement tracking to tenant_departures
-- Enforces two-step offline cash departure settlement and auditability.

ALTER TABLE tenant_departures
    ADD COLUMN IF NOT EXISTS settlement_mode TEXT NOT NULL DEFAULT 'payout'
        CHECK (settlement_mode IN ('payout', 'offline_cash')),
    ADD COLUMN IF NOT EXISTS cash_receipt_ref TEXT,
    ADD COLUMN IF NOT EXISTS cash_disbursed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS cash_disbursed_by UUID REFERENCES users(id);

CREATE INDEX IF NOT EXISTS idx_tenant_departures_settlement_mode
    ON tenant_departures (property_id, settlement_mode)
    WHERE settlement_mode = 'offline_cash';

