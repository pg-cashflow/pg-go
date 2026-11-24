SET lock_timeout = '5s';

-- Migration 126: KYC Active States Unique Index Backstop
-- Standard: ASD-STE100
-- Purpose:
--   Enforce at the database level that a tenant can have at most one active KYC verification across
--   ALL active states: ('pending', 'request_unknown', 'pending_manual_review', 'verified').
--
-- Safety Invariant:
--   Never auto-expire unresolved records (e.g. request_unknown, pending_manual_review) to force
--   index creation. If duplicate active verifications exist, report them and abort immediately
--   so an operator can reconcile them via an audited process.

DO $$
DECLARE
    dup_count INT;
    dup_tenants TEXT;
BEGIN
    SELECT COUNT(*), string_agg(tenant_id::text, ', ')
    INTO dup_count, dup_tenants
    FROM (
        SELECT tenant_id
        FROM kyc_verification
        WHERE status IN ('pending', 'request_unknown', 'pending_manual_review', 'verified')
        GROUP BY tenant_id
        HAVING COUNT(*) > 1
    ) dups;

    IF dup_count > 0 THEN
        RAISE EXCEPTION 'Migration 126 aborted: detected % tenant(s) with multiple active KYC verifications (tenants: %). Active records across (pending, request_unknown, pending_manual_review, verified) must be audited and reconciled before creating idx_kyc_one_active_per_tenant.', dup_count, dup_tenants;
    END IF;
END $$;

DROP INDEX IF EXISTS idx_kyc_one_active_per_tenant;

CREATE UNIQUE INDEX IF NOT EXISTS idx_kyc_one_active_per_tenant 
    ON kyc_verification(tenant_id) 
    WHERE status IN ('pending', 'request_unknown', 'pending_manual_review', 'verified');
