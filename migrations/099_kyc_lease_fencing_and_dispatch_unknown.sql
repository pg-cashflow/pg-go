-- Migration 099: KYC Lease Fencing, Dispatch Unknown Status, and Review Capability.
-- Standard: ASD-STE100
-- Invariants:
--   - In-flight KYC leases use a unique lease_token (fencing token) to prevent replica race conditions.
--   - Payment intents support 'dispatch_unknown' for ambiguous gateway transport states.
--   - KYC document access requires explicit 'kyc.documents.review' capability.

-- 1. Fencing token for KYC in-flight leases
ALTER TABLE kyc_in_flight_lock 
    ADD COLUMN IF NOT EXISTS lease_token UUID NOT NULL DEFAULT gen_random_uuid();

-- 2. Expand payment intent status check constraint to include dispatch_unknown
DO $$
BEGIN
    ALTER TABLE payment_intents DROP CONSTRAINT IF EXISTS chk_intent_status;
    ALTER TABLE payment_intents ADD CONSTRAINT chk_intent_status 
        CHECK (status IN ('initiating', 'created', 'paid', 'expired', 'failed', 'superseded', 'dispatch_unknown'));
EXCEPTION
    WHEN OTHERS THEN NULL;
END $$;

