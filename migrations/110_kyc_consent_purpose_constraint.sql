-- KYC-04: restrict kyc_consent.purpose to server-owned allowlist values
ALTER TABLE kyc_consent
    ADD CONSTRAINT kyc_consent_purpose_check
    CHECK (purpose IN (
        'residency_verification_and_kyc',
        'tenant_verification',
        'rental_onboarding',
        'aadhaar_kyc',
        'onboarding',
        'onboarding_kyc'
    ));
