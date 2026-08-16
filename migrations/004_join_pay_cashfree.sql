-- Tenant-first join, payment reports, Cashfree-ready collector flag.

ALTER TABLE properties
  ADD COLUMN IF NOT EXISTS invite_code VARCHAR(12) UNIQUE,
  ADD COLUMN IF NOT EXISTS payment_mode VARCHAR(20) NOT NULL DEFAULT 'manual';

UPDATE properties
SET invite_code = UPPER(SUBSTRING(REPLACE(gen_random_uuid()::text, '-', '') FROM 1 FOR 8))
WHERE invite_code IS NULL;

ALTER TABLE properties
  ALTER COLUMN invite_code SET NOT NULL;

ALTER TABLE properties
  ADD CONSTRAINT chk_properties_payment_mode CHECK (payment_mode IN ('manual', 'cashfree'));

CREATE TABLE join_requests (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id    UUID NOT NULL REFERENCES properties(id),
  user_id        UUID NOT NULL REFERENCES users(id),
  phone          VARCHAR(15) NOT NULL,
  name           VARCHAR(100) NOT NULL DEFAULT '',
  aadhaar_last4  CHAR(4),
  status         VARCHAR(20) NOT NULL DEFAULT 'pending',
  tenant_id      UUID REFERENCES tenants(id),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT chk_join_status CHECK (status IN ('pending', 'approved', 'rejected'))
);
CREATE INDEX idx_join_requests_property_status ON join_requests(property_id, status);
CREATE UNIQUE INDEX uq_join_pending_phone ON join_requests(property_id, phone) WHERE status = 'pending';

CREATE TABLE payment_reports (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  due_id       UUID NOT NULL REFERENCES dues(id),
  tenant_id    UUID NOT NULL REFERENCES tenants(id),
  property_id  UUID NOT NULL REFERENCES properties(id),
  upi_txn_id   VARCHAR(50) NOT NULL,
  amount       INTEGER NOT NULL,
  image_bytes  BYTEA,
  status       VARCHAR(20) NOT NULL DEFAULT 'pending_review',
  reported_by  UUID NOT NULL REFERENCES users(id),
  reviewed_by  UUID REFERENCES users(id),
  reviewed_at  TIMESTAMPTZ,
  note         TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT chk_report_status CHECK (status IN ('pending_review', 'confirmed', 'rejected')),
  CONSTRAINT uq_payment_reports_upi UNIQUE (upi_txn_id)
);
CREATE INDEX idx_payment_reports_property_status ON payment_reports(property_id, status);
CREATE INDEX idx_payment_reports_due ON payment_reports(due_id);

CREATE TABLE payment_intents (
  id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  due_id              UUID NOT NULL REFERENCES dues(id),
  provider            VARCHAR(20) NOT NULL DEFAULT 'cashfree',
  provider_order_id   VARCHAR(80) NOT NULL UNIQUE,
  payment_session_id  TEXT,
  amount_paise        INTEGER NOT NULL,
  status              VARCHAR(20) NOT NULL DEFAULT 'created',
  expires_at          TIMESTAMPTZ,
  cf_payment_id       VARCHAR(80) UNIQUE,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT chk_intent_status CHECK (status IN ('created', 'paid', 'expired', 'failed'))
);
CREATE INDEX idx_payment_intents_due ON payment_intents(due_id);
CREATE INDEX idx_payment_intents_open ON payment_intents(status, created_at) WHERE status = 'created';
