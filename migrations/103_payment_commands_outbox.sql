SET lock_timeout = '5s';

-- Migration 103: Dedicated Payment Commands Outbox for External Gateway Operations
-- Standard: ASD-STE100
-- Reference: Decouple external network operations from internal accounting projections

CREATE TABLE IF NOT EXISTS payment_commands (
    id BIGSERIAL PRIMARY KEY,
    command_type TEXT NOT NULL,
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE RESTRICT,
    source_id UUID NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key TEXT NOT NULL UNIQUE,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'processing', 'completed', 'failed', 'dead_letter')),
    attempt_count INT NOT NULL DEFAULT 0,
    max_attempts INT NOT NULL DEFAULT 5,
    last_error TEXT,
    locked_until TIMESTAMPTZ,
    next_retry_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_payment_commands_pending
    ON payment_commands (next_retry_at, id)
    WHERE status IN ('pending', 'processing');

CREATE INDEX IF NOT EXISTS idx_payment_commands_source
    ON payment_commands (source_id, command_type);
