-- Migration 023: Transactional Ledger Outbox Events for Financial Mirror Resilience (ADR-012/Track E)
--
-- Tables added:
--   ledger_outbox_events  - written inside domain transactions (e.g. SettleDepartureUnderLock)
--                           before commit; asynchronous worker reads and posts to double-entry ledger.

CREATE TABLE IF NOT EXISTS ledger_outbox_events (
    id BIGSERIAL PRIMARY KEY,
    event_type TEXT NOT NULL,
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    source_id UUID NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key TEXT NOT NULL UNIQUE,
    attempt_count INT NOT NULL DEFAULT 0,
    max_attempts INT NOT NULL DEFAULT 5,
    last_error TEXT,
    next_retry_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ
);

-- Partial index for active pending events ordered by retry schedule
CREATE INDEX IF NOT EXISTS idx_ledger_outbox_pending
    ON ledger_outbox_events (next_retry_at, id)
    WHERE processed_at IS NULL AND failed_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_ledger_outbox_source
    ON ledger_outbox_events (source_id, event_type);
