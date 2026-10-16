-- Migration 075: Add channel and destination columns to otp_requests
-- Standard: ASD-STE100
-- Method: Empirical First-Principles Verification

ALTER TABLE otp_requests
  ADD COLUMN IF NOT EXISTS channel VARCHAR(16) NOT NULL DEFAULT 'sms',
  ADD COLUMN IF NOT EXISTS destination VARCHAR(255);

CREATE INDEX IF NOT EXISTS idx_otp_destination_purpose
  ON otp_requests(destination, purpose) WHERE used = FALSE;
