-- Migration 034: Add immutable family_started_at to refresh_tokens for un-resettable 90-day session ceiling
ALTER TABLE refresh_tokens ADD COLUMN IF NOT EXISTS family_started_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- Backfill existing tokens with their family's earliest created_at timestamp
UPDATE refresh_tokens r
SET family_started_at = f.m
FROM (
    SELECT family_id, MIN(created_at) AS m
    FROM refresh_tokens
    GROUP BY family_id
) f
WHERE r.family_id = f.family_id;

CREATE INDEX IF NOT EXISTS idx_refresh_tokens_family_started ON refresh_tokens(family_id, family_started_at);

