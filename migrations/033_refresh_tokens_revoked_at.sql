-- Migration 033: Add revoked_at and indexes to refresh_tokens for safe atomic rotation & replay detection
ALTER TABLE refresh_tokens ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_family_revoked ON refresh_tokens(family_id, revoked, expires_at);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_expires_at ON refresh_tokens(expires_at);

-- Enforce append-only immutability for settlement balance runs audit table
CREATE OR REPLACE FUNCTION prevent_modification_daily_settlement_balance_runs()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'daily_settlement_balance_runs is an immutable append-only audit table';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_prevent_modification ON daily_settlement_balance_runs;
CREATE TRIGGER trg_prevent_modification
BEFORE UPDATE ON daily_settlement_balance_runs
FOR EACH ROW EXECUTE FUNCTION prevent_modification_daily_settlement_balance_runs();
