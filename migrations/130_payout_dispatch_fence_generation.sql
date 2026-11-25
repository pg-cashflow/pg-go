-- Migration 130: Add monotonic dispatch_fence_generation and dispatch_lease_token to payout_batches.
-- Ensures stale payout workers cannot mutate batch or item state after lease expiration or takeover.

SET lock_timeout = '5s';

ALTER TABLE payout_batches
    ADD COLUMN IF NOT EXISTS dispatch_fence_generation BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS dispatch_lease_token UUID;

COMMENT ON COLUMN payout_batches.dispatch_fence_generation IS
    'Monotonically increasing fencing generation incremented each time a worker acquires the batch dispatch lease.';
COMMENT ON COLUMN payout_batches.dispatch_lease_token IS
    'Active resource_leases.lease_token for the current dispatch worker; cleared on release or replaced on takeover.';
