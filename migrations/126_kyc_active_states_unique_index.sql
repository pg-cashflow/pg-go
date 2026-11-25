SET lock_timeout = '5s';

-- Migration 126: KYC Active States Unique Index Backstop
-- Standard: ASD-STE100
-- Purpose:
--   Enforce at the database level that a tenant can have at most one active KYC verification across
--   ALL active states: ('pending', 'request_unknown', 'pending_manual_review', 'verified').

-- Preflight: abort if any tenant has multiple active KYC records.
-- Do not auto-expire verified or active identity history; resolve duplicates explicitly first.
DO $$
DECLARE
    v_dup_tenants BIGINT;
BEGIN
    SELECT COUNT(*) INTO v_dup_tenants
    FROM (
        SELECT tenant_id
        FROM kyc_verification
        WHERE status IN ('pending', 'request_unknown', 'pending_manual_review', 'verified')
        GROUP BY tenant_id
        HAVING COUNT(*) > 1
    ) dup;

    IF v_dup_tenants > 0 THEN
        RAISE EXCEPTION 'Migration 126 aborted: found % tenant(s) with multiple active kyc_verification rows. Run preflight duplicate report and resolve explicitly before creating unique index.', v_dup_tenants;
    END IF;
END $$;

DROP INDEX IF EXISTS idx_kyc_one_active_per_tenant;

CREATE UNIQUE INDEX IF NOT EXISTS idx_kyc_one_active_per_tenant 
    ON kyc_verification(tenant_id) 
    WHERE status IN ('pending', 'request_unknown', 'pending_manual_review', 'verified');
