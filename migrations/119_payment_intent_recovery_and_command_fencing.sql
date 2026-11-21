SET lock_timeout = '5s';

-- Migration 119: Payment Intent Invariant and Payment Command Lease Fencing
-- Standard: ASD-STE100

-- 1. Pre-migration deduplication: supersede duplicate active intents before enforcing uniqueness
WITH ranked_active_intents AS (
    SELECT id, due_id,
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
)
UPDATE payment_intents
SET status = 'superseded',
    updated_at = NOW()
WHERE id IN (
    SELECT id FROM ranked_active_intents WHERE rn > 1
);

WITH ranked_active_dues_items AS (
    SELECT id, due_id,
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
SET status = 'superseded'
WHERE id IN (
    SELECT id FROM ranked_active_dues_items WHERE rn > 1
);

-- 2. Enforce unique index for active intents
DROP INDEX IF EXISTS uq_active_intent_due;
CREATE UNIQUE INDEX uq_active_intent_due 
    ON payment_intents(due_id) 
    WHERE status IN ('initiating', 'created', 'dispatch_unknown');

DROP INDEX IF EXISTS uq_active_intent_dues_item;
CREATE UNIQUE INDEX uq_active_intent_dues_item 
    ON payment_intent_dues(due_id) 
    WHERE status IN ('initiating', 'created', 'dispatch_unknown');

-- 3. Add fence_token to payment_commands for distributed worker fencing
ALTER TABLE payment_commands
    ADD COLUMN IF NOT EXISTS fence_token BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_payment_commands_fencing
    ON payment_commands (id, fence_token, status)
    WHERE status = 'processing';
