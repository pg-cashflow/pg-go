-- Migration 067: Widen phone columns to VARCHAR(16) for E.164 compliance and add calendar token revocation.
--
-- 1. International E.164 phone numbers contain '+' followed by up to 15 digits (total 16 characters).
--    Widen VARCHAR(15) phone columns to VARCHAR(16) to prevent SQL truncation errors on 15-digit numbers.
--    In PostgreSQL, widening VARCHAR length is a metadata-only catalog update with zero table rewrite.
-- 2. Add revoked_at column to property_calendar_tokens to support explicit revocation (REQ-SEC-004).

SET LOCAL lock_timeout = '5s';

ALTER TABLE properties ALTER COLUMN owner_phone TYPE VARCHAR(16);
ALTER TABLE tenants ALTER COLUMN phone TYPE VARCHAR(16);
ALTER TABLE tenants ALTER COLUMN guardian_phone TYPE VARCHAR(16);
ALTER TABLE users ALTER COLUMN phone TYPE VARCHAR(16);
ALTER TABLE otp_requests ALTER COLUMN phone TYPE VARCHAR(16);
ALTER TABLE join_requests ALTER COLUMN phone TYPE VARCHAR(16);
ALTER TABLE staff_profiles ALTER COLUMN phone TYPE VARCHAR(16);
ALTER TABLE referrals ALTER COLUMN phone TYPE VARCHAR(16);

-- Calendar tokens revocation support (REQ-SEC-004)
ALTER TABLE property_calendar_tokens ADD COLUMN IF NOT EXISTS revoked_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_calendar_tokens_revoked ON property_calendar_tokens (property_id, revoked_at);
