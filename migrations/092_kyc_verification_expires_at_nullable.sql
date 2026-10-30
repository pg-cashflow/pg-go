SET lock_timeout = '5s';

-- Migration 092: Make expires_at nullable for verified KYC records
-- Standard: ASD-STE100
-- Tenancy KYC is a one-time onboarding identity check and does not expire
-- unless a positive validity window is explicitly configured.

ALTER TABLE kyc_verification DROP CONSTRAINT IF EXISTS chk_kyc_verified_fields;

ALTER TABLE kyc_verification ADD CONSTRAINT chk_kyc_verified_fields CHECK (
    status <> 'verified' OR (
        verified_at IS NOT NULL AND 
        identity_hash IS NOT NULL AND 
        masked_uid IS NOT NULL
    )
);

