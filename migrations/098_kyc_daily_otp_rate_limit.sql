-- Migration 098: Per-tenant Aadhaar OTP rate limiting and daily quota enforcement.
-- Standard: ASD-STE100
-- Invariants:
--   - Hard database limit on billed UIDAI OTP requests per tenant per 24 hours.
--   - Serialized via FOR UPDATE on tenant row to prevent concurrent race abuse.

CREATE TABLE IF NOT EXISTS kyc_otp_attempt_log (
    id         BIGSERIAL PRIMARY KEY,
    tenant_id  UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_kyc_otp_attempt_lookup
    ON kyc_otp_attempt_log(tenant_id, created_at);

