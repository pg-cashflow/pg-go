-- 063: repair processed_webhook_events for fresh installs.
--
-- Root cause (found by TestE2EMoneyLifecycle_DoubleEntryConservation and
-- TestLivePostgresMigration020AndRepository on a freshly migrated database):
--   * 018 created processed_webhook_events with (dedup_key PK, provider,
--     event_type, received_at).
--   * 020 uses CREATE TABLE IF NOT EXISTS, so on a database where 018 ran
--     first its CREATE is a no-op. created_at was never added.
--   * dedup_key has no default, and the repository never supplies it.
--   Result: every RecordProcessedEvent INSERT fails (missing created_at column,
--   then NOT NULL on dedup_key), so PAYMENT_SETTLED webhooks return 500
--   "settle failed" and no payment is ever recorded.
--
-- Migrations are checksummed and must never be edited once applied, so the
-- repair is additive and idempotent. It is safe on databases that already
-- have these columns.

ALTER TABLE processed_webhook_events
    ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- dedup_key is the legacy primary key. Dedup is enforced by the unique index
-- uq_processed_event (provider, event_type, provider_reference_id, event_status),
-- so a random key is sufficient for rows written by the current repository.
ALTER TABLE processed_webhook_events
    ALTER COLUMN dedup_key SET DEFAULT (gen_random_uuid()::text);
