-- Migration 008: Transactional outbox, durable notifications, and delivery bookkeeping.
--
-- Tables added:
--   outbox_events           – written in the same DB txn as domain writes; dispatcher reads this.
--   notifications           – durable in-app notification record; source of truth for the bell.
--   notification_deliveries – per-channel idempotency and retry bookkeeping.
--
-- See: docs/notifications.md (Phase A of the notifications rollout).

CREATE TABLE outbox_events (
  id            BIGSERIAL PRIMARY KEY,
  event_type    TEXT NOT NULL,
  property_id   UUID NOT NULL,
  tenant_id     UUID,
  actor_role    TEXT NOT NULL,
  payload       JSONB NOT NULL DEFAULT '{}'::jsonb,
  attempt_count INT NOT NULL DEFAULT 0,
  last_error    TEXT,
  next_retry_at TIMESTAMPTZ,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  dispatched_at TIMESTAMPTZ,
  failed_at     TIMESTAMPTZ
);

-- Partial index covering only active, eligible events ordered by retry schedule.
-- Rows with dispatched_at or failed_at set are excluded entirely.
CREATE INDEX idx_outbox_pending ON outbox_events (next_retry_at, id)
  WHERE dispatched_at IS NULL AND failed_at IS NULL;

CREATE TABLE notifications (
  id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  -- source_event_id links back to the outbox event for idempotency:
  --   UNIQUE (source_event_id, recipient_id) + ON CONFLICT DO NOTHING prevents
  --   duplicate notification rows if the dispatcher crashes and retries.
  source_event_id     BIGINT REFERENCES outbox_events(id) ON DELETE SET NULL,
  recipient_id        UUID NOT NULL,
  property_id         UUID NOT NULL,
  type                TEXT NOT NULL,
  title               TEXT NOT NULL,
  deep_link           TEXT NOT NULL,
  is_action_required  BOOLEAN NOT NULL DEFAULT false,
  read_at             TIMESTAMPTZ,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT uq_notifications_source_recipient UNIQUE (source_event_id, recipient_id)
);

-- Compound index for cursor-based pagination (created_at DESC, id DESC).
CREATE INDEX idx_notifications_recipient_cursor ON notifications (recipient_id, created_at DESC, id DESC);

CREATE TABLE notification_deliveries (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  notification_id  UUID NOT NULL REFERENCES notifications(id) ON DELETE CASCADE,
  -- CHECK constraints catch typo'd enum values at the DB layer.
  channel          TEXT NOT NULL CHECK (channel IN ('inapp', 'push', 'whatsapp', 'sms', 'email')),
  status           TEXT NOT NULL CHECK (status IN ('pending', 'sent', 'failed', 'skipped_pref')),
  attempt_count    INT NOT NULL DEFAULT 0,
  last_error       TEXT,
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- Idempotency key: one delivery row per notification per channel.
  CONSTRAINT uq_notification_delivery_channel UNIQUE (notification_id, channel)
);
