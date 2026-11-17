SET lock_timeout = '5s';

-- Migration 113: Add document_type to kyc_verification for non-Aadhaar verification
-- Standard: ASD-STE100
-- Invariants:
--   - Supports voluntary identification routes (PAN, passport, voter ID, driving licence, Aadhaar) per Aadhaar Act.
--   - Defaults to 'aadhaar' for backwards compatibility.

ALTER TABLE kyc_verification
    ADD COLUMN IF NOT EXISTS document_type TEXT DEFAULT 'aadhaar';
