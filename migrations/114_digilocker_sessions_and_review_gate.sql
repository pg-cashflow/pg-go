SET lock_timeout = '5s';

-- Migration 114: Add DigiLocker session reference and landlord manual review payment toggle
-- Standard: ASD-STE100
-- Invariants:
--   - Stores Cashfree reference_id alongside vendor_reference_id for O(1) dual lookup.
--   - Default allow_pay_during_manual_review is FALSE (fail closed: unverified tenants cannot pay).

ALTER TABLE kyc_verification
    ADD COLUMN IF NOT EXISTS cf_reference_id TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_kyc_cf_reference_id 
    ON kyc_verification(cf_reference_id) 
    WHERE cf_reference_id IS NOT NULL;

ALTER TABLE property_onboarding_settings
    ADD COLUMN IF NOT EXISTS allow_pay_during_manual_review BOOLEAN NOT NULL DEFAULT FALSE;
