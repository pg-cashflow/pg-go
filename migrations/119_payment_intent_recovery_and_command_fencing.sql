SET lock_timeout = '5s';

-- Migration 119: Payment Intent Invariant, Quarantine Deduplication, and Payment Command Lease Fencing
-- Standard: ASD-STE100

-- 1. Extend payment intent status check constraint to permit 'needs_review' for quarantined records
ALTER TABLE payment_intents DROP CONSTRAINT IF EXISTS chk_intent_status;
ALTER TABLE payment_intents ADD CONSTRAINT chk_intent_status 
    CHECK (status IN ('initiating', 'created', 'paid', 'expired', 'failed', 'superseded', 'dispatch_unknown', 'needs_review'));

-- 2. Audit quarantine table for unresolved duplicate intents
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

-- 3. Pre-migration deduplication: quarantine unresolved duplicate active intents for manual inspection
WITH duplicate_active_intents AS (
    SELECT id, due_id, provider_order_id, amount_paise, status,
           ROW_NUMBER() OVER (
               PARTITION BY due_id
               ORDER BY
                   CASE status
                       WHEN 'created' THEN 1
                       WHEN 'initiating' THEN 2
                       WHEN 'dispatch_unknown' THEN 3
                       ELSE 4
                   END,
                   updated_at DESC,
                   created_at DESC
           ) as rn
    FROM payment_intents
    WHERE status IN ('initiating', 'created', 'dispatch_unknown')
      AND due_id IS NOT NULL
),
quarantined AS (
    INSERT INTO payment_intent_quarantine (payment_intent_id, due_id, provider_order_id, amount_paise, original_status, quarantine_reason)
    SELECT id, due_id, provider_order_id, amount_paise, status,
           'duplicate active intent quarantined during migration 119 pre-check'
    FROM duplicate_active_intents
    WHERE rn > 1
    RETURNING payment_intent_id
)
UPDATE payment_intents
SET status = 'needs_review',
    updated_at = NOW()
WHERE id IN (SELECT payment_intent_id FROM quarantined);

-- Multi-due duplicate item quarantine
WITH duplicate_active_dues AS (
    SELECT id, payment_intent_id, due_id, status,
           ROW_NUMBER() OVER (
               PARTITION BY due_id
               ORDER BY
                   CASE status
                       WHEN 'created' THEN 1
                       WHEN 'initiating' THEN 2
                       WHEN 'dispatch_unknown' THEN 3
                       ELSE 4
                   END,
                   created_at DESC
           ) as rn
    FROM payment_intent_dues
    WHERE status IN ('initiating', 'created', 'dispatch_unknown')
)
UPDATE payment_intent_dues
SET status = 'needs_review'
WHERE id IN (
    SELECT id FROM duplicate_active_dues WHERE rn > 1
);

-- 4. Enforce unique index for active intents (including needs_review to prevent duplicate checkout during quarantine)
DROP INDEX IF EXISTS uq_active_intent_due;
CREATE UNIQUE INDEX uq_active_intent_due 
    ON payment_intents(due_id) 
    WHERE status IN ('initiating', 'created', 'dispatch_unknown', 'needs_review');

DROP INDEX IF EXISTS uq_active_intent_dues_item;
CREATE UNIQUE INDEX uq_active_intent_dues_item 
    ON payment_intent_dues(due_id) 
    WHERE status IN ('initiating', 'created', 'dispatch_unknown', 'needs_review');

-- 5. Add fence_token to payment_commands for distributed worker fencing
ALTER TABLE payment_commands
    ADD COLUMN IF NOT EXISTS fence_token BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_payment_commands_fencing
    ON payment_commands (id, fence_token, status)
    WHERE status = 'processing';

-- 6. Add persistent idempotency_key to payment_intents for stable provider retries
ALTER TABLE payment_intents
    ADD COLUMN IF NOT EXISTS idempotency_key UUID NOT NULL DEFAULT gen_random_uuid();

CREATE INDEX IF NOT EXISTS idx_payment_intents_idempotency
    ON payment_intents (idempotency_key);
