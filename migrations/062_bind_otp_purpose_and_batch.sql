-- Migration 062: Bind OTP requests to purpose and optional batch_id
-- Standard: ASD-STE100
-- Method: Empirical First-Principles Verification

ALTER TABLE otp_requests
  ADD COLUMN IF NOT EXISTS purpose VARCHAR(32) NOT NULL DEFAULT 'login',
  ADD COLUMN IF NOT EXISTS batch_id UUID;

CREATE INDEX IF NOT EXISTS idx_otp_phone_purpose ON otp_requests(phone, purpose) WHERE used = FALSE;
