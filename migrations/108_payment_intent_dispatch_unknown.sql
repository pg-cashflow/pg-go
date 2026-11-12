SET lock_timeout = '5s';

-- Migration 108: Payment Intent Dispatch Unknown Status and Guard Indices
-- Standard: ASD-STE100
-- Invariants:
--   - Payment intents support 'dispatch_unknown' for ambiguous gateway transport states.
--   - Active intent unique indices protect dues during 'dispatch_unknown' to prevent duplicate checkout.

-- 1. Update check constraint to support dispatch_unknown
ALTER TABLE payment_intents DROP CONSTRAINT IF EXISTS chk_intent_status;
ALTER TABLE payment_intents ADD CONSTRAINT chk_intent_status 
    CHECK (status IN ('initiating', 'created', 'paid', 'expired', 'failed', 'superseded', 'dispatch_unknown'));

-- 2. Expand unique constraints to prevent double payments during ambiguous dispatch
DROP INDEX IF EXISTS uq_active_intent_due;
CREATE UNIQUE INDEX uq_active_intent_due 
    ON payment_intents(due_id) 
    WHERE status IN ('initiating', 'created', 'dispatch_unknown');

DROP INDEX IF EXISTS uq_active_intent_dues_item;
CREATE UNIQUE INDEX uq_active_intent_dues_item 
    ON payment_intent_dues(due_id) 
    WHERE status IN ('initiating', 'created', 'dispatch_unknown');
