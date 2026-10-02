-- Migration 033: Add revoked_at and indexes to refresh_tokens for safe atomic rotation & replay detection
ALTER TABLE refresh_tokens ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_family_revoked ON refresh_tokens(family_id, revoked, expires_at);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_expires_at ON refresh_tokens(expires_at);

