SET lock_timeout = '5s';

-- Migration 126: KYC Active States Unique Index Backstop
-- Standard: ASD-STE100
-- Purpose:
--   Enforce at the database level that a tenant can have at most one active KYC verification across
--   ALL active states: ('pending', 'request_unknown', 'pending_manual_review', 'verified').

DROP INDEX IF EXISTS idx_kyc_one_active_per_tenant;

CREATE UNIQUE INDEX IF NOT EXISTS idx_kyc_one_active_per_tenant 
    ON kyc_verification(tenant_id) 
    WHERE status IN ('pending', 'request_unknown', 'pending_manual_review', 'verified');
