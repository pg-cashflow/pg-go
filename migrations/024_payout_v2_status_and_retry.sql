-- Migration 024: Payout Phase 2 (Cashfree Transfers V2 Integration)
-- Extends payout_batches and payout_items status enums and adds retry/tracking columns

-- 1. Extend payout_batches status CHECK constraint to include 'dispatch_unknown' and 'failed'
ALTER TABLE payout_batches
    DROP CONSTRAINT IF EXISTS payout_batches_status_check;

ALTER TABLE payout_batches
    ADD CONSTRAINT payout_batches_status_check
    CHECK (status IN ('draft', 'approved', 'processing', 'dispatch_unknown', 'completed', 'partially_failed', 'failed', 'cancelled'));

-- 2. Extend payout_items status CHECK constraint to include 'processing', 'cashfree_approval_pending', 'retriable_failed', 'reversed'
ALTER TABLE payout_items
    DROP CONSTRAINT IF EXISTS payout_items_status_check;

ALTER TABLE payout_items
    ADD CONSTRAINT payout_items_status_check
    CHECK (status IN (
        'pending',
        'processing',
        'cashfree_approval_pending',
        'succeeded',
        'failed',
        'retriable_failed',
        'rejected',
        'reversed',
        'cancelled'
    ));

-- 3. Add Cashfree Transfer ID and retry parent reference to payout_items
ALTER TABLE payout_items
    ADD COLUMN IF NOT EXISTS cf_transfer_id VARCHAR(100),
    ADD COLUMN IF NOT EXISTS retry_of UUID REFERENCES payout_items(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_payout_items_cf_transfer_id ON payout_items(cf_transfer_id) WHERE cf_transfer_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_payout_items_retry_of ON payout_items(retry_of) WHERE retry_of IS NOT NULL;

-- 4. Update partial unique index on tenant_departures to account for in-flight payout statuses
DROP INDEX IF EXISTS uq_payout_departure_active;
CREATE UNIQUE INDEX IF NOT EXISTS uq_payout_departure_active 
    ON payout_items (departure_id) 
    WHERE departure_id IS NOT NULL AND status IN ('pending', 'processing', 'cashfree_approval_pending', 'succeeded');
