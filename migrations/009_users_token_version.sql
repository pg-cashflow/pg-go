-- Migration 009: per-user JWT token version for session revocation.
-- Incrementing token_version invalidates previously issued app JWTs on the next request.

ALTER TABLE users
  ADD COLUMN IF NOT EXISTS token_version INTEGER NOT NULL DEFAULT 1;
