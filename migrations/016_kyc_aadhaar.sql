-- Migration 016: Tenant Identity (Cashfree Secure ID DigiLocker & Secure QR)
-- Strict DPDP Act 2023 compliance, immutable audit ledger, single source of truth.

-- 1. Consent tracking (DPDP Rule 8 compliant)
CREATE TABLE IF NOT EXISTS kyc_consent (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id          UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    purpose            TEXT NOT NULL,
    consent_version    VARCHAR(20) NOT NULL DEFAULT 'v1',
    consent_text       TEXT NOT NULL,
    consent_text_hash  CHAR(64) NOT NULL,
    consent_given_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ip_address         INET,
    user_agent         TEXT,
    revoked_at         TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_kyc_consent_tenant ON kyc_consent(tenant_id);

-- 2. Verification records
CREATE TABLE IF NOT EXISTS kyc_verification (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id           UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    consent_id          UUID NOT NULL REFERENCES kyc_consent(id),
    vendor_name         TEXT NOT NULL DEFAULT 'cashfree_secure_id',
    vendor_reference_id TEXT NOT NULL UNIQUE,
    masked_uid          CHAR(4),
    identity_hash       TEXT,
    hash_key_version    SMALLINT NOT NULL DEFAULT 1,
    is_dedupable        BOOLEAN NOT NULL DEFAULT TRUE,
    duplicate_detected  BOOLEAN NOT NULL DEFAULT FALSE,
    method              TEXT NOT NULL CHECK (method IN ('digilocker', 'ocr', 'qr')),
    status              TEXT NOT NULL CHECK (status IN ('pending', 'verified', 'failed', 'expired', 'revoked')),
    failure_reason      TEXT,
    verified_at         TIMESTAMPTZ,
    expires_at          TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_kyc_verified_fields CHECK (
        status <> 'verified' OR (
            verified_at IS NOT NULL AND 
            expires_at IS NOT NULL AND 
            identity_hash IS NOT NULL AND 
            masked_uid IS NOT NULL
        )
    )
);
ALTER TABLE kyc_verification ADD COLUMN IF NOT EXISTS duplicate_detected BOOLEAN NOT NULL DEFAULT FALSE;
CREATE UNIQUE INDEX IF NOT EXISTS idx_kyc_one_active_per_tenant 
    ON kyc_verification(tenant_id) WHERE status IN ('pending', 'verified');
CREATE INDEX IF NOT EXISTS idx_kyc_dedup_hash 
    ON kyc_verification(identity_hash) WHERE status = 'verified' AND is_dedupable = TRUE;

-- 3. Tamper-evident, hash-chained audit log
CREATE TABLE IF NOT EXISTS kyc_audit_log (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   UUID NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    actor       TEXT NOT NULL,
    action      TEXT NOT NULL,
    detail_hash TEXT,
    prev_hash   TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_kyc_audit_tenant_id ON kyc_audit_log(tenant_id, id);

-- 4. Trigger: strictly prevent tampering or deletions on audit log
CREATE OR REPLACE FUNCTION trg_kyc_audit_log_immutable()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'kyc_audit_log entries are immutable and cannot be updated or deleted';
END;
$$ LANGUAGE plpgsql;

DO $$ BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_trigger WHERE tgname = 'trg_kyc_audit_immutable'
    ) THEN
        CREATE TRIGGER trg_kyc_audit_immutable
        BEFORE UPDATE OR DELETE ON kyc_audit_log
        FOR EACH ROW EXECUTE FUNCTION trg_kyc_audit_log_immutable();
    END IF;
END $$;
