-- Migration 052: Add replaced_by tracking to refresh_tokens for replay-safe idempotent rotation
ALTER TABLE refresh_tokens ADD COLUMN IF NOT EXISTS replaced_by UUID REFERENCES refresh_tokens(id);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_replaced_by ON refresh_tokens(replaced_by) WHERE replaced_by IS NOT NULL;
