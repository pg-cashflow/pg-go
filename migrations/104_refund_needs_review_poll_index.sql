SET lock_timeout = '5s';

-- Migration 104: Include needs_review in gateway_refunds pending poll index
-- Standard: ASD-STE100
-- Reference: Ensure stale-refund polling queries can use index when scanning for needs_review

DROP INDEX IF EXISTS idx_refunds_pending;

CREATE INDEX idx_refunds_pending ON gateway_refunds (created_at ASC)
WHERE status IN ('initiated', 'pending', 'on_hold', 'needs_review');
