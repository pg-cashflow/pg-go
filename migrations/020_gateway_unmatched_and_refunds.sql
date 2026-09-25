-- Migration 020: Unmatched Gateway Receipts, Multi-Due Allocations & Idempotent Refunds

-- 1. Raw Webhook Ingestion & Invariant Audit Log (Captures all arrivals before parsing)
CREATE TABLE IF NOT EXISTS webhook_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider TEXT NOT NULL DEFAULT 'cashfree',
    event_type TEXT,
    provider_reference_id TEXT,
    event_status TEXT,
    signature TEXT,
    timestamp_header TEXT,
    headers JSONB,
    raw_payload JSONB NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ,
    processing_status TEXT NOT NULL DEFAULT 'received' CHECK (processing_status IN ('received', 'processed', 'ignored', 'dead_letter', 'failed', 'unmatched')),
    error_message TEXT
);
ALTER TABLE webhook_events
    ADD COLUMN IF NOT EXISTS signature TEXT,
    ADD COLUMN IF NOT EXISTS timestamp_header TEXT;
CREATE INDEX IF NOT EXISTS idx_webhook_events_ref ON webhook_events(provider, event_type, provider_reference_id);
CREATE INDEX IF NOT EXISTS idx_webhook_events_dead_letter ON webhook_events(processing_status) WHERE processing_status = 'dead_letter';

-- 2. Actionable Domain Unmatched Receipts (Provider-Neutral, Captures validated payments with domain conflicts)
CREATE TABLE IF NOT EXISTS unmatched_gateway_receipts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID REFERENCES properties(id) ON DELETE SET NULL,
    payment_intent_id UUID REFERENCES payment_intents(id) ON DELETE SET NULL,
    due_id UUID REFERENCES dues(id) ON DELETE SET NULL,
    provider TEXT NOT NULL DEFAULT 'cashfree',
    provider_order_id TEXT NOT NULL,
    provider_payment_id TEXT NOT NULL,
    cf_payment_id TEXT, -- Backward-compatible mirror for Cashfree adapter
    amount_paise BIGINT NOT NULL,
    currency TEXT NOT NULL DEFAULT 'INR',
    payment_method TEXT,
    failure_reason TEXT NOT NULL CHECK (failure_reason IN ('amount_mismatch', 'unknown_order', 'due_already_settled', 'session_superseded', 'duplicate_payment', 'payment_not_found')),
    raw_payload JSONB NOT NULL,
    resolved_at TIMESTAMPTZ,
    resolved_by UUID REFERENCES users(id),
    resolution_action TEXT CHECK (resolution_action IS NULL OR resolution_action IN ('refunded', 'applied_as_credit', 'written_off')),
    resolution_notes TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_unmatched_provider_payment UNIQUE (provider, provider_payment_id)
);
ALTER TABLE unmatched_gateway_receipts
    ADD COLUMN IF NOT EXISTS provider TEXT NOT NULL DEFAULT 'cashfree',
    ADD COLUMN IF NOT EXISTS provider_order_id TEXT,
    ADD COLUMN IF NOT EXISTS provider_payment_id TEXT;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'uq_unmatched_provider_payment') THEN
        ALTER TABLE unmatched_gateway_receipts ADD CONSTRAINT uq_unmatched_provider_payment UNIQUE (provider, provider_payment_id);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_unmatched_provider_id_sync') THEN
        ALTER TABLE unmatched_gateway_receipts ADD CONSTRAINT chk_unmatched_provider_id_sync CHECK (
            (provider = 'cashfree' AND cf_payment_id IS NOT NULL AND provider_payment_id IS NOT NULL AND cf_payment_id = provider_payment_id)
            OR
            (provider <> 'cashfree' AND cf_payment_id IS NULL)
        ) NOT VALID;
    END IF;
END $$;
CREATE INDEX IF NOT EXISTS idx_unmatched_provider_payment ON unmatched_gateway_receipts(provider, provider_payment_id);
CREATE INDEX IF NOT EXISTS idx_unmatched_unresolved ON unmatched_gateway_receipts(property_id) WHERE resolved_at IS NULL;

-- 3. Modify Payments Table for Gateway Payment Time, Payer Type, Multi-Due, and Provider-Neutral Tracking
ALTER TABLE payments 
    ALTER COLUMN due_id DROP NOT NULL,
    ADD COLUMN IF NOT EXISTS provider TEXT NOT NULL DEFAULT 'cashfree',
    ADD COLUMN IF NOT EXISTS provider_payment_id TEXT,
    ADD COLUMN IF NOT EXISTS payer_type TEXT CHECK (payer_type IN ('tenant', 'guardian')) DEFAULT 'tenant',
    ADD COLUMN IF NOT EXISTS gateway_payment_time TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS cf_payment_id TEXT UNIQUE,
    ADD COLUMN IF NOT EXISTS is_unapplied BOOLEAN NOT NULL DEFAULT FALSE;

-- Safe Historical Backfill before Constraints
UPDATE payments 
SET provider = CASE 
    WHEN matched_by IN ('cash', 'manual') THEN 'cash'
    WHEN matched_by = 'deposit_netting' THEN 'internal'
    WHEN matched_by IN ('due_code', 'amount_date_window') THEN 'bank'
    WHEN cf_payment_id IS NOT NULL OR provider_payment_id IS NOT NULL THEN 'cashfree'
    ELSE 'legacy_unverified'
END
WHERE provider = 'cashfree' AND (cf_payment_id IS NULL OR provider_payment_id IS NULL);

UPDATE payments 
SET provider_payment_id = cf_payment_id 
WHERE provider = 'cashfree' AND provider_payment_id IS NULL AND cf_payment_id IS NOT NULL;

UPDATE payments 
SET cf_payment_id = provider_payment_id 
WHERE provider = 'cashfree' AND cf_payment_id IS NULL AND provider_payment_id IS NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'uq_payments_provider_payment') THEN
        ALTER TABLE payments ADD CONSTRAINT uq_payments_provider_payment UNIQUE (provider, provider_payment_id);
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_payments_matched_by') THEN
        ALTER TABLE payments ADD CONSTRAINT chk_payments_matched_by 
            CHECK (matched_by IN ('due_code', 'amount_date_window', 'manual', 'cash', 'cashfree', 'deposit_netting')) NOT VALID;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_payments_provider_id_sync') THEN
        ALTER TABLE payments ADD CONSTRAINT chk_payments_provider_id_sync CHECK (
            (provider = 'cashfree' AND cf_payment_id IS NOT NULL AND provider_payment_id IS NOT NULL AND cf_payment_id = provider_payment_id)
            OR
            (provider <> 'cashfree' AND cf_payment_id IS NULL)
        ) NOT VALID;
    END IF;
END $$;

-- 4. Payment Allocations Table (Many-to-One between Payments and Dues, Financial Restrict)
CREATE TABLE IF NOT EXISTS payment_allocations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    payment_id UUID NOT NULL REFERENCES payments(id) ON DELETE RESTRICT,
    due_id UUID NOT NULL REFERENCES dues(id) ON DELETE RESTRICT,
    amount_paise BIGINT NOT NULL CHECK (amount_paise > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_payment_due_alloc UNIQUE (payment_id, due_id)
);
CREATE INDEX IF NOT EXISTS idx_payment_allocations_due ON payment_allocations(due_id);
CREATE INDEX IF NOT EXISTS idx_payment_allocations_payment ON payment_allocations(payment_id);

-- 5. Duplicate Billing Guard on Dues
CREATE UNIQUE INDEX IF NOT EXISTS uq_dues_tenant_rent_cycle 
    ON dues(tenant_id, period_start) WHERE kind = 'rent';

-- 6. Support Multi-Due Payment Intents & Double-Active Guard
ALTER TABLE payment_intents
    ALTER COLUMN due_id DROP NOT NULL;

DO $$
BEGIN
    ALTER TABLE payment_intents DROP CONSTRAINT IF EXISTS chk_intent_status;
    ALTER TABLE payment_intents ADD CONSTRAINT chk_intent_status 
        CHECK (status IN ('initiating', 'created', 'paid', 'expired', 'failed', 'superseded'));
EXCEPTION
    WHEN OTHERS THEN NULL;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS uq_active_intent_due 
    ON payment_intents(due_id) WHERE status IN ('initiating', 'created');

CREATE TABLE IF NOT EXISTS payment_intent_dues (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    payment_intent_id UUID NOT NULL REFERENCES payment_intents(id) ON DELETE CASCADE,
    due_id UUID NOT NULL REFERENCES dues(id) ON DELETE RESTRICT,
    amount_paise BIGINT NOT NULL CHECK (amount_paise > 0),
    allocation_order INT NOT NULL DEFAULT 1,
    -- ARCHITECTURAL INVARIANT: status is initialized on INSERT and mutated EXCLUSIVELY
    -- via trg_sync_payment_intent_dues_status from payment_intents.status.
    -- Direct application UPDATEs on this column are strictly prohibited to prevent guard desync.
    status VARCHAR(20) NOT NULL DEFAULT 'initiating',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_intent_due UNIQUE (payment_intent_id, due_id)
);
ALTER TABLE payment_intent_dues
    ADD COLUMN IF NOT EXISTS status VARCHAR(20) NOT NULL DEFAULT 'initiating';
COMMENT ON COLUMN payment_intent_dues.status IS 'Mirrors payment_intents.status exclusively via trg_sync_payment_intent_dues_status. Never update directly.';

CREATE UNIQUE INDEX IF NOT EXISTS uq_active_intent_dues_item
    ON payment_intent_dues(due_id) WHERE status IN ('initiating', 'created');

CREATE INDEX IF NOT EXISTS idx_intent_dues_intent ON payment_intent_dues(payment_intent_id);
DROP INDEX IF EXISTS idx_payment_intents_poller;
CREATE INDEX IF NOT EXISTS idx_payment_intents_poller ON payment_intents(status, expires_at) WHERE status IN ('created', 'superseded');

CREATE OR REPLACE FUNCTION sync_payment_intent_dues_status()
RETURNS TRIGGER AS $$
BEGIN
    UPDATE payment_intent_dues
    SET status = NEW.status
    WHERE payment_intent_id = NEW.id;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_sync_payment_intent_dues_status ON payment_intents;
CREATE TRIGGER trg_sync_payment_intent_dues_status
AFTER UPDATE OF status ON payment_intents
FOR EACH ROW
EXECUTE FUNCTION sync_payment_intent_dues_status();

CREATE OR REPLACE FUNCTION prevent_direct_payment_intent_dues_status_update()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.status IS DISTINCT FROM NEW.status AND pg_trigger_depth() <= 1 THEN
        RAISE EXCEPTION 'payment_intent_dues.status is trigger-managed and cannot be updated directly';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_prevent_direct_payment_intent_dues_status_update ON payment_intent_dues;
CREATE TRIGGER trg_prevent_direct_payment_intent_dues_status_update
BEFORE UPDATE OF status ON payment_intent_dues
FOR EACH ROW
EXECUTE FUNCTION prevent_direct_payment_intent_dues_status_update();

-- 7. Idempotent Payment Gateway Refunds Ledger (Provider-Neutral)
CREATE TABLE IF NOT EXISTS gateway_refunds (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    payment_id UUID NOT NULL REFERENCES payments(id),
    property_id UUID NOT NULL REFERENCES properties(id),
    provider TEXT NOT NULL DEFAULT 'cashfree',
    provider_refund_id TEXT,
    refund_reference TEXT, -- Nullable for cashfree_auto and dashboard refunds
    idempotency_key VARCHAR(128),
    amount_paise BIGINT NOT NULL CHECK (amount_paise > 0),
    status TEXT NOT NULL CHECK (status IN ('initiated', 'pending', 'on_hold', 'succeeded', 'failed', 'cancelled')),
    cf_refund_id TEXT UNIQUE, -- Backward-compatible mirror for Cashfree adapter
    reason TEXT NOT NULL,
    source TEXT NOT NULL CHECK (source IN ('owner', 'system', 'cashfree_auto', 'dashboard')) DEFAULT 'owner',
    initiated_by UUID REFERENCES users(id),
    raw_response JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_refund_idempotency UNIQUE (payment_id, idempotency_key),
    CONSTRAINT chk_refund_initiator CHECK (
        (source = 'owner' AND initiated_by IS NOT NULL) OR
        (source <> 'owner' AND initiated_by IS NULL)
    )
);
ALTER TABLE gateway_refunds
    ADD COLUMN IF NOT EXISTS provider TEXT NOT NULL DEFAULT 'cashfree',
    ADD COLUMN IF NOT EXISTS provider_refund_id TEXT;
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'uq_refunds_provider_refund') THEN
        ALTER TABLE gateway_refunds ADD CONSTRAINT uq_refunds_provider_refund UNIQUE (provider, provider_refund_id);
    END IF;
    ALTER TABLE gateway_refunds DROP CONSTRAINT IF EXISTS chk_refunds_provider_id_sync;
    ALTER TABLE gateway_refunds ADD CONSTRAINT chk_refunds_provider_id_sync CHECK (
        (provider = 'cashfree' AND (
            (cf_refund_id IS NULL AND provider_refund_id IS NULL) OR
            (cf_refund_id IS NOT NULL AND provider_refund_id IS NOT NULL AND cf_refund_id = provider_refund_id)
        ))
        OR
        (provider <> 'cashfree' AND cf_refund_id IS NULL)
    ) NOT VALID;
END $$;
CREATE INDEX IF NOT EXISTS idx_refunds_payment ON gateway_refunds(payment_id);
CREATE INDEX IF NOT EXISTS idx_refunds_pending ON gateway_refunds(status) WHERE status IN ('initiated', 'pending', 'on_hold');
CREATE UNIQUE INDEX IF NOT EXISTS uq_refunds_reference 
    ON gateway_refunds (refund_reference) WHERE refund_reference IS NOT NULL;

-- 8. Refund Allocations Table (Reverse Attribution to Dues, Financial Restrict)
CREATE TABLE IF NOT EXISTS refund_allocations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    refund_id UUID NOT NULL REFERENCES gateway_refunds(id) ON DELETE RESTRICT,
    due_id UUID NOT NULL REFERENCES dues(id) ON DELETE RESTRICT,
    amount_paise BIGINT NOT NULL CHECK (amount_paise > 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_refund_due_alloc UNIQUE (refund_id, due_id)
);
CREATE INDEX IF NOT EXISTS idx_refund_allocations_due ON refund_allocations(due_id);
CREATE INDEX IF NOT EXISTS idx_refund_allocations_refund ON refund_allocations(refund_id);

-- 9. Atomic Webhook Deduplication Table (Non-destructive schema evolution from Migration 018)
CREATE TABLE IF NOT EXISTS processed_webhook_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider TEXT NOT NULL DEFAULT 'cashfree',
    event_type TEXT NOT NULL,             -- e.g. 'PAYMENT_SETTLED', 'REFUND_STATUS_WEBHOOK', 'AUTO_REFUND_STATUS_WEBHOOK'
    provider_reference_id TEXT NOT NULL DEFAULT '',
    event_status TEXT NOT NULL DEFAULT '', -- e.g. 'SUCCESS', 'FAILED', 'CANCELLED', or empty string if single-shot
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
ALTER TABLE processed_webhook_events
    ADD COLUMN IF NOT EXISTS provider_reference_id TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS event_status TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS uq_processed_event 
    ON processed_webhook_events (provider, event_type, provider_reference_id, event_status);

-- 10. Backfill legacy payment_allocations from historical payments
-- NOTE: In 001_initial.sql, payments.amount is defined as INTEGER NOT NULL in paise (not rupees).
-- Therefore, p.amount::BIGINT represents exact integer paise with zero scaling required.
INSERT INTO payment_allocations (payment_id, due_id, amount_paise, created_at)
SELECT p.id, p.due_id, p.amount::BIGINT, p.created_at
FROM payments p
WHERE p.due_id IS NOT NULL
ON CONFLICT (payment_id, due_id) DO NOTHING;

-- 11. Dual-Write Trigger: Auto-populate payment_allocations for single-due payments
CREATE OR REPLACE FUNCTION trg_payments_auto_allocate_single_due()
RETURNS TRIGGER AS $$
BEGIN
    IF NEW.due_id IS NOT NULL AND NEW.amount > 0 AND NOT NEW.is_unapplied THEN
        INSERT INTO payment_allocations (payment_id, due_id, amount_paise, created_at)
        VALUES (NEW.id, NEW.due_id, NEW.amount::BIGINT, NEW.created_at)
        ON CONFLICT (payment_id, due_id) DO UPDATE SET amount_paise = EXCLUDED.amount_paise;
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_payments_auto_allocate ON payments;
CREATE TRIGGER trg_payments_auto_allocate
AFTER INSERT ON payments
FOR EACH ROW
EXECUTE FUNCTION trg_payments_auto_allocate_single_due();
