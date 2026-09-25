-- Migration 019: Owner Operational Settlement & Payout Ledger with Line-Item Audit Trail

-- Property settlement / accounting distribution ledger
CREATE TABLE IF NOT EXISTS payout_records (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id        UUID NOT NULL REFERENCES properties(id),
    owner_user_id      UUID NOT NULL REFERENCES users(id),
    period_start       DATE NOT NULL,
    period_end         DATE NOT NULL,
    gross_amount_paise BIGINT NOT NULL CHECK (gross_amount_paise >= 0),
    platform_fee_paise BIGINT NOT NULL DEFAULT 0 CHECK (platform_fee_paise >= 0),
    net_amount_paise   BIGINT NOT NULL CHECK (net_amount_paise >= 0),
    status             VARCHAR(20) NOT NULL DEFAULT 'pending' 
                       CHECK (status IN ('pending', 'transferred', 'failed', 'cancelled')),
    transfer_method    VARCHAR(30) NOT NULL DEFAULT 'manual_bank_transfer' 
                       CHECK (transfer_method IN ('manual_bank_transfer', 'cashfree_payout')),
    transfer_reference VARCHAR(100),
    marked_by          UUID REFERENCES users(id),
    marked_at          TIMESTAMPTZ,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_payout_net_amount CHECK (net_amount_paise = gross_amount_paise - platform_fee_paise)
);
CREATE INDEX IF NOT EXISTS idx_payouts_property_period ON payout_records(property_id, period_start, period_end);
CREATE INDEX IF NOT EXISTS idx_payouts_owner_status ON payout_records(owner_user_id, status);

-- Line-item audit trail linking distributions directly to exact dues and settled payments
CREATE TABLE IF NOT EXISTS payout_record_items (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    payout_record_id UUID NOT NULL REFERENCES payout_records(id) ON DELETE CASCADE,
    due_id           UUID NOT NULL REFERENCES dues(id),
    payment_id       UUID NOT NULL REFERENCES payments(id),
    amount_paise     BIGINT NOT NULL CHECK (amount_paise > 0),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_payout_item_payment UNIQUE (payment_id)
);
CREATE INDEX IF NOT EXISTS idx_payout_items_record ON payout_record_items(payout_record_id);
