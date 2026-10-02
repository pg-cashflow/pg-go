-- Migration 034: Add immutable family_started_at to refresh_tokens for un-resettable 90-day session ceiling
ALTER TABLE refresh_tokens ADD COLUMN IF NOT EXISTS family_started_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
UPDATE refresh_tokens SET family_started_at = created_at WHERE family_started_at IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_family_started ON refresh_tokens(family_id, family_started_at);
