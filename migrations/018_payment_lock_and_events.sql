-- Migration 018: Payment Order Distributed Lease Lock & Gateway Webhook Event Deduplication Ledger

-- Distributed lease lock for payment order / intent creation (mirrors kyc_in_flight_lock)
CREATE TABLE IF NOT EXISTS payment_in_flight_lock (
    due_id     UUID PRIMARY KEY REFERENCES dues(id) ON DELETE CASCADE,
    locked_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_payment_lock_expires ON payment_in_flight_lock(expires_at);

-- Gateway webhook event deduplication ledger
CREATE TABLE IF NOT EXISTS processed_webhook_events (
    dedup_key   VARCHAR(128) PRIMARY KEY,
    provider    VARCHAR(32) NOT NULL,
    event_type  VARCHAR(64) NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_processed_events_provider ON processed_webhook_events(provider, received_at DESC);
