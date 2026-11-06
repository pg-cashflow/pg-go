SET lock_timeout = '5s';

-- Migration 101: Payee verification metadata for penny-drop bank verification
-- Standard: ASD-STE100
-- Reference: ADR-022

ALTER TABLE payout_payees
    ADD COLUMN IF NOT EXISTS verification_status TEXT NOT NULL DEFAULT 'unverified',
    ADD COLUMN IF NOT EXISTS verification_method TEXT,
    ADD COLUMN IF NOT EXISTS verification_ref TEXT,
    ADD COLUMN IF NOT EXISTS name_at_bank TEXT,
    ADD COLUMN IF NOT EXISTS name_match_score NUMERIC(5,2),
    ADD COLUMN IF NOT EXISTS name_match_result TEXT;

UPDATE payout_payees
SET verification_status = 'owner_attested',
    verification_method = 'owner_attestation'
WHERE is_verified = TRUE AND (verification_status IS NULL OR verification_status = 'unverified');
