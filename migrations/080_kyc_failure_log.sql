-- Migration 080: Per-attempt failure log for Aadhaar OTP verification.
-- Enables state machine to detect repeated terminal conditions (e.g., repeated INVALID_AADHAAR).

CREATE TABLE IF NOT EXISTS kyc_failure_log (
    id           BIGSERIAL PRIMARY KEY,
    tenant_id    UUID NOT NULL REFERENCES tenants(id),
    method       TEXT NOT NULL,
    reason_code  TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_kyc_failure_log_lookup
    ON kyc_failure_log(tenant_id, reason_code, created_at);
