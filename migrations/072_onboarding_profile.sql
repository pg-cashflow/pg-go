SET lock_timeout = '5s';

-- Migration 072: Typed tenant onboarding profile (P0-C).

ALTER TABLE property_onboarding_settings
  ADD COLUMN IF NOT EXISTS require_kyc_before_pay BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS join_pending_ttl_days INT NOT NULL DEFAULT 30;

ALTER TABLE join_requests
  ADD COLUMN IF NOT EXISTS occupation_type TEXT,
  ADD COLUMN IF NOT EXISTS date_of_birth DATE,
  ADD COLUMN IF NOT EXISTS profile_schema_version INT NOT NULL DEFAULT 1,
  ADD COLUMN IF NOT EXISTS occupation_details JSONB NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN IF NOT EXISTS emergency_contact_name VARCHAR(100) NOT NULL DEFAULT '';

ALTER TABLE join_requests DROP CONSTRAINT IF EXISTS chk_join_occupation_type;
ALTER TABLE join_requests ADD CONSTRAINT chk_join_occupation_type CHECK (
  occupation_type IS NULL OR occupation_type IN ('student', 'working_professional', 'other')
);

ALTER TABLE tenants
  ADD COLUMN IF NOT EXISTS occupation_type TEXT,
  ADD COLUMN IF NOT EXISTS date_of_birth DATE,
  ADD COLUMN IF NOT EXISTS occupation_details JSONB NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN IF NOT EXISTS emergency_contact_name VARCHAR(100) NOT NULL DEFAULT '';

ALTER TABLE tenants DROP CONSTRAINT IF EXISTS chk_tenants_occupation_type;
ALTER TABLE tenants ADD CONSTRAINT chk_tenants_occupation_type CHECK (
  occupation_type IS NULL OR occupation_type IN ('student', 'working_professional', 'other')
);

CREATE TABLE IF NOT EXISTS join_idempotency (
    idempotency_key TEXT NOT NULL,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    response_body JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, idempotency_key)
);

ALTER TABLE join_idempotency ENABLE ROW LEVEL SECURITY;
ALTER TABLE join_idempotency FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS join_idempotency_maint ON join_idempotency;
CREATE POLICY join_idempotency_maint ON join_idempotency
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true) WITH CHECK (true);

GRANT SELECT, INSERT ON join_idempotency TO pgapp_app, pgapp_maint, pgapp_worker;
