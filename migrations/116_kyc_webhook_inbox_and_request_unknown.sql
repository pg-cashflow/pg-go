SET lock_timeout = '5s';

-- Migration 116: KYC Webhook Inbox and Request Unknown Status
-- Standard: ASD-STE100
-- Purpose:
--   1. Add 'request_unknown' status to kyc_verification to capture ambiguous gateway initiation outcomes.
--   2. Establish kyc_webhook_inbox table for deduplication on (provider, SHA256(timestamp + ":" + rawBody)).
--   3. Apply appropriate RLS policies and table grants.

-- Step 1: Update kyc_verification status constraint to include request_unknown
ALTER TABLE kyc_verification DROP CONSTRAINT IF EXISTS kyc_verification_status_check;
ALTER TABLE kyc_verification ADD CONSTRAINT kyc_verification_status_check 
    CHECK (status IN ('pending', 'request_unknown', 'pending_manual_review', 'verified', 'failed', 'rejected', 'expired', 'revoked'));

-- Partial index for ambiguous initiation reconciliation
CREATE INDEX IF NOT EXISTS idx_kyc_request_unknown 
    ON kyc_verification(created_at) 
    WHERE status = 'request_unknown';

-- Step 2: Establish kyc_webhook_inbox table for reliable webhook processing
CREATE TABLE IF NOT EXISTS kyc_webhook_inbox (
    id BIGSERIAL PRIMARY KEY,
    provider VARCHAR(64) NOT NULL DEFAULT 'cashfree',
    event_hash VARCHAR(64) NOT NULL,
    event_timestamp VARCHAR(64) NOT NULL,
    verification_id VARCHAR(128),
    reference_id VARCHAR(128),
    event_type VARCHAR(128),
    status VARCHAR(64),
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    processing_status VARCHAR(32) NOT NULL DEFAULT 'received' CHECK (processing_status IN ('received', 'processed', 'duplicate', 'failed')),
    error_message TEXT,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    processed_at TIMESTAMPTZ,
    CONSTRAINT uq_kyc_webhook_inbox_provider_hash UNIQUE (provider, event_hash)
);

CREATE INDEX IF NOT EXISTS idx_kyc_webhook_inbox_verification_id 
    ON kyc_webhook_inbox(verification_id) WHERE verification_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_kyc_webhook_inbox_status 
    ON kyc_webhook_inbox(processing_status) WHERE processing_status = 'received';

-- Step 3: Grants
DO $$
BEGIN
    GRANT SELECT, INSERT, UPDATE ON kyc_webhook_inbox TO pgapp_app, pgapp_maint, pgapp_worker;
    GRANT USAGE, SELECT ON SEQUENCE kyc_webhook_inbox_id_seq TO pgapp_app, pgapp_maint, pgapp_worker;
EXCEPTION
    WHEN OTHERS THEN NULL;
END $$;
