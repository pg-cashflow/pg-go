SET lock_timeout = '5s';

-- Migration 106: Lease tokens and expiry for ledger outbox worker resilience (ADR-023)
-- Standard: ASD-STE100
-- Invariants:
--   - External dispatch calls must occur outside database transactions.
--   - Outbox events in-flight are leased with a fencing lease_token and lease_expires_at.

ALTER TABLE ledger_outbox_events
    ADD COLUMN IF NOT EXISTS lease_token UUID,
    ADD COLUMN IF NOT EXISTS lease_expires_at TIMESTAMPTZ;

-- Partial index for claiming pending or expired-lease outbox events
CREATE INDEX IF NOT EXISTS idx_ledger_outbox_claim
    ON ledger_outbox_events (next_retry_at, id)
    WHERE processed_at IS NULL AND failed_at IS NULL;
