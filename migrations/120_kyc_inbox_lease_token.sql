SET lock_timeout = '5s';

-- Migration 120: KYC Webhook Inbox Lease Fencing, Payment Intent Recovery and Idempotency
-- Standard: ASD-STE100
-- Purpose:
--   1. Add claim_token to kyc_webhook_inbox for distributed lease fencing (KYC-LEASE-01).
--   2. Extend payment intent status check constraint to permit 'needs_review' for quarantined records.
--   3. Create payment_intent_quarantine table for unresolved duplicate intents.
--   4. Update unique index for active intents including needs_review to prevent duplicate checkout.
--   5. Add persistent idempotency_key to payment_intents for stable provider retries.

-- 1. Add claim_token to kyc_webhook_inbox (KYC-LEASE-01)
ALTER TABLE kyc_webhook_inbox
    ADD COLUMN IF NOT EXISTS claim_token VARCHAR(64);

CREATE INDEX IF NOT EXISTS idx_kyc_webhook_inbox_claim_token
    ON kyc_webhook_inbox (claim_token)
    WHERE claim_token IS NOT NULL;

-- 2. Extend payment intent status check constraint to permit 'needs_review'
ALTER TABLE payment_intents DROP CONSTRAINT IF EXISTS chk_intent_status;
ALTER TABLE payment_intents ADD CONSTRAINT chk_intent_status 
    CHECK (status IN ('initiating', 'created', 'paid', 'expired', 'failed', 'superseded', 'dispatch_unknown', 'needs_review'));

-- 3. Audit quarantine table for unresolved duplicate intents
CREATE TABLE IF NOT EXISTS payment_intent_quarantine (
    id BIGSERIAL PRIMARY KEY,
    payment_intent_id UUID NOT NULL REFERENCES payment_intents(id) ON DELETE CASCADE,
    due_id UUID,
    provider_order_id VARCHAR(80) NOT NULL,
    amount_paise INTEGER NOT NULL,
    original_status VARCHAR(20) NOT NULL,
    quarantine_reason TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 4. Update unique index for active intents to include needs_review
DROP INDEX IF EXISTS uq_active_intent_due;
CREATE UNIQUE INDEX uq_active_intent_due 
    ON payment_intents(due_id) 
    WHERE status IN ('initiating', 'created', 'dispatch_unknown', 'needs_review');

DROP INDEX IF EXISTS uq_active_intent_dues_item;
CREATE UNIQUE INDEX uq_active_intent_dues_item 
    ON payment_intent_dues(due_id) 
    WHERE status IN ('initiating', 'created', 'dispatch_unknown', 'needs_review');

-- 5. Add persistent idempotency_key to payment_intents
ALTER TABLE payment_intents
    ADD COLUMN IF NOT EXISTS idempotency_key UUID NOT NULL DEFAULT gen_random_uuid();

CREATE INDEX IF NOT EXISTS idx_payment_intents_idempotency
    ON payment_intents (idempotency_key);
