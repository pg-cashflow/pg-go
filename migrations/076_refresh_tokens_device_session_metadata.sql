-- Migration 076: Add device metadata and last_used_at to refresh_tokens for per-device active sessions
-- Standard: ASD-STE100
-- Method: Empirical First-Principles Verification

ALTER TABLE refresh_tokens
  ADD COLUMN IF NOT EXISTS user_agent TEXT,
  ADD COLUMN IF NOT EXISTS ip_address VARCHAR(45),
  ADD COLUMN IF NOT EXISTS last_used_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

CREATE INDEX IF NOT EXISTS idx_refresh_tokens_active_sessions
  ON refresh_tokens(user_id, family_id, expires_at)
  WHERE revoked = FALSE;
