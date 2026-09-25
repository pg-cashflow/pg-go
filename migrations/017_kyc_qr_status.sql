-- Migration 017: KYC QR status, attested photo, name mismatch, and distributed in-flight lock.

ALTER TABLE kyc_verification
  ADD COLUMN IF NOT EXISTS qr_status TEXT
    CHECK (qr_status IS NULL OR qr_status IN ('SECURE', 'PRIMITIVE', 'NOT_PRESENT', 'UNPROCESSABLE')),
  ADD COLUMN IF NOT EXISTS name_mismatch BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS attested_photo_bytes BYTEA,
  ADD COLUMN IF NOT EXISTS photo_stored BOOLEAN NOT NULL DEFAULT FALSE;

-- Distributed lease table for multi-instance Cloud Run concurrency & double-billing protection
CREATE TABLE IF NOT EXISTS kyc_in_flight_lock (
  tenant_id UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
  locked_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
