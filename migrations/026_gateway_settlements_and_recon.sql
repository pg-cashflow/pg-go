-- Migration 026: Gateway Settlements & Order-to-Intent Reconciliation (Track N / Ticket 13)

CREATE TABLE IF NOT EXISTS gateway_settlements (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID REFERENCES properties(id) ON DELETE SET NULL,
    cf_settlement_id TEXT NOT NULL,
    order_id TEXT,                                      -- Nullable for aggregate batch webhooks
    cf_payment_id TEXT,                                 -- Gateway payment ID
    payment_intent_id UUID REFERENCES payment_intents(id) ON DELETE SET NULL,
    payment_id UUID REFERENCES payments(id) ON DELETE SET NULL,
    ingestion_source VARCHAR(20) NOT NULL DEFAULT 'webhook'
        CHECK (ingestion_source IN ('webhook', 'order_fetch', 'csv_import')),
    utr TEXT NOT NULL DEFAULT '',                       -- Bank reference / UTR / transfer_id
    currency VARCHAR(3) NOT NULL DEFAULT 'INR',
    gross_amount_paise BIGINT NOT NULL,                -- Payment amount collected from tenant
    service_charge_paise BIGINT NOT NULL DEFAULT 0,    -- Gateway TDR / processing fee
    service_tax_paise BIGINT NOT NULL DEFAULT 0,       -- GST on processing fee (18%)
    adjustment_paise BIGINT NOT NULL DEFAULT 0,        -- Refunds, chargebacks, dispute debits/credits netted
    net_amount_paise BIGINT NOT NULL,                  -- Actual amount credited to merchant bank
    settlement_status VARCHAR(30) NOT NULL,             -- SUCCESS, PENDING, FAILED, REVERSED
    settled_on TIMESTAMPTZ,                             -- Webhook batch bank execution timestamp
    settlement_initiated_on TIMESTAMPTZ,                -- Webhook batch initiation timestamp
    transfer_time TIMESTAMPTZ,                          -- Order-level transfer timestamp
    reconciliation_status VARCHAR(30) NOT NULL DEFAULT 'unmatched'
        CHECK (reconciliation_status IN ('matched', 'unmatched', 'discrepancy', 'manually_reconciled')),
    discrepancy_reason TEXT,                            -- arithmetic_imbalance, intent_not_found, amount_mismatch, payment_missing
    journal_entry_id UUID REFERENCES financial_journal_entries(id) ON DELETE SET NULL,
    resolution_notes TEXT,
    resolved_by UUID REFERENCES users(id) ON DELETE SET NULL,
    resolved_at TIMESTAMPTZ,
    raw_payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Unique index using COALESCE for expression-based uniqueness across nullable order_id and cf_payment_id
CREATE UNIQUE INDEX IF NOT EXISTS uq_gateway_settlement_record 
    ON gateway_settlements (cf_settlement_id, COALESCE(order_id, ''), COALESCE(cf_payment_id, ''));

CREATE INDEX IF NOT EXISTS idx_gateway_settlements_prop_status 
    ON gateway_settlements (property_id, reconciliation_status);

CREATE INDEX IF NOT EXISTS idx_gateway_settlements_order_id 
    ON gateway_settlements (order_id) WHERE order_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_gateway_settlements_cf_settlement 
    ON gateway_settlements (cf_settlement_id);

CREATE INDEX IF NOT EXISTS idx_gateway_settlements_utr 
    ON gateway_settlements (utr) WHERE utr != '';
