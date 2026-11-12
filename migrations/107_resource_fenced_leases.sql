SET lock_timeout = '5s';

-- Migration 107: Distributed Resource Leases with Monotonic Fencing Generations
-- Standard: ASD-STE100
-- Invariants:
--   - Replaces PostgreSQL session-level advisory locks across transaction-pooled endpoints.
--   - Tracks unique lease_token ownership and monotonically increasing generation counters.
--   - Mutual exclusion is guaranteed across horizontal worker replicas and connection restarts.

CREATE TABLE IF NOT EXISTS resource_leases (
    resource_type TEXT NOT NULL,
    resource_id   TEXT NOT NULL,
    lease_owner   TEXT NOT NULL,
    lease_token   UUID NOT NULL,
    generation    BIGINT NOT NULL DEFAULT 1,
    acquired_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at    TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (resource_type, resource_id)
);

CREATE INDEX IF NOT EXISTS idx_resource_leases_expiry 
    ON resource_leases (expires_at);
