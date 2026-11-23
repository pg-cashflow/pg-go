-- Migration 117: Drop dead KYC and pay settings from property_onboarding_settings
-- KYC before payment is mandatory platform-wide; manual review does not grant payment access.

SET lock_timeout = '5s';

ALTER TABLE property_onboarding_settings
    DROP COLUMN IF EXISTS allow_pay_during_manual_review,
    DROP COLUMN IF EXISTS require_kyc_before_pay;
