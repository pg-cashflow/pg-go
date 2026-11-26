SET lock_timeout = '5s';

-- Migration 126: KYC Active States Unique Index Backstop
-- Standard: ASD-STE100
-- Purpose:
--   Enforce at the database level that a tenant can have at most one active KYC verification across
--   ALL active states: ('pending', 'request_unknown', 'pending_manual_review', 'verified').

-- Preflight: resolve any existing duplicate active records before creating the unique index.
WITH ranked AS (
    SELECT id, tenant_id, status,
           ROW_NUMBER() OVER (
               PARTITION BY tenant_id
               ORDER BY 
                   CASE status
                       WHEN 'verified' THEN 1
                       WHEN 'pending_manual_review' THEN 2
                       WHEN 'pending' THEN 3
                       WHEN 'request_unknown' THEN 4
                       ELSE 5
                   END,
                   updated_at DESC,
                   created_at DESC
           ) as rn
    FROM kyc_verification
    WHERE status IN ('pending', 'request_unknown', 'pending_manual_review', 'verified')
)
UPDATE kyc_verification kv
SET status = 'expired',
    failure_reason = COALESCE(failure_reason, 'superseded during active state constraint consolidation'),
    updated_at = NOW()
FROM ranked r
WHERE kv.id = r.id AND r.rn > 1;

DROP INDEX IF EXISTS idx_kyc_one_active_per_tenant;

CREATE UNIQUE INDEX IF NOT EXISTS idx_kyc_one_active_per_tenant 
    ON kyc_verification(tenant_id) 
    WHERE status IN ('pending', 'request_unknown', 'pending_manual_review', 'verified');
