-- Migration 081: Add document_bytes and document_content_type to kyc_verification
-- Standard: ASD-STE100
-- Invariants:
--   - Stores uploaded manual review document binary (image/PDF up to 5MB)
--   - Cleared on consent revocation per DPDP Rule 8

SET lock_timeout = '2s';

ALTER TABLE kyc_verification
    ADD COLUMN IF NOT EXISTS document_bytes BYTEA,
    ADD COLUMN IF NOT EXISTS document_content_type TEXT;
