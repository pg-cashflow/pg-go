SET lock_timeout = '5s';

-- Migration 077: Add manual review workflow and Aadhaar OTP method to kyc_verification
-- Standard: ASD-STE100
-- Invariants:
--   - Government and cryptographic authentications (DigiLocker, Aadhaar OTP, UIDAI Secure QR) auto-verify.
--   - Manual document uploads (OCR) require human review and transition to pending_manual_review.

-- 1. Update status CHECK constraint
ALTER TABLE kyc_verification DROP CONSTRAINT IF EXISTS kyc_verification_status_check;
ALTER TABLE kyc_verification ADD CONSTRAINT kyc_verification_status_check 
    CHECK (status IN ('pending', 'pending_manual_review', 'verified', 'failed', 'rejected', 'expired', 'revoked'));

-- 2. Update method CHECK constraint
ALTER TABLE kyc_verification DROP CONSTRAINT IF EXISTS kyc_verification_method_check;
ALTER TABLE kyc_verification ADD CONSTRAINT kyc_verification_method_check 
    CHECK (method IN ('digilocker', 'aadhaar_otp', 'ocr', 'qr'));

-- 3. Add manual review fields
ALTER TABLE kyc_verification
    ADD COLUMN IF NOT EXISTS reviewed_by UUID REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS reviewed_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS rejection_reason TEXT,
    ADD COLUMN IF NOT EXISTS ocr_data JSONB,
    ADD COLUMN IF NOT EXISTS document_url TEXT;

-- 4. Partial index for owner/manager review queue
CREATE INDEX IF NOT EXISTS idx_kyc_pending_review 
    ON kyc_verification(tenant_id, created_at) 
    WHERE status = 'pending_manual_review';
