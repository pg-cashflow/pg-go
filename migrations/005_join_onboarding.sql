-- Invite-in, dashboard-now onboarding: profile fields, ID photo, pending_allocation.

ALTER TABLE join_requests
  ADD COLUMN IF NOT EXISTS permanent_address TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS current_address TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS parent_name VARCHAR(100) NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS emergency_phone VARCHAR(20) NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS joined_on DATE;

ALTER TABLE tenants
  ADD COLUMN IF NOT EXISTS permanent_address TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS current_address TEXT NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS parent_name VARCHAR(100) NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS emergency_phone VARCHAR(20) NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS joined_on DATE,
  ADD COLUMN IF NOT EXISTS id_photo_bytes BYTEA,
  ADD COLUMN IF NOT EXISTS has_id_photo BOOLEAN NOT NULL DEFAULT FALSE;

-- Allow null due_day for pending_allocation only.
ALTER TABLE tenants ALTER COLUMN due_day DROP NOT NULL;

ALTER TABLE tenants DROP CONSTRAINT IF EXISTS chk_tenants_due_day;
ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_due_day_check;

ALTER TABLE tenants
  ADD CONSTRAINT chk_tenants_due_day CHECK (
    due_day IS NULL OR (due_day BETWEEN 1 AND 28)
  );

ALTER TABLE tenants DROP CONSTRAINT IF EXISTS chk_tenants_status;
ALTER TABLE tenants DROP CONSTRAINT IF EXISTS chk_tenants_rent_status;

ALTER TABLE tenants
  ADD CONSTRAINT chk_tenants_status CHECK (
    status IN ('pending_allocation', 'active', 'vacated')
  );

ALTER TABLE tenants
  ADD CONSTRAINT chk_tenants_rent_status CHECK (
    (status = 'pending_allocation' AND rent_amount = 0 AND due_day IS NULL)
    OR (status IN ('active', 'vacated') AND rent_amount > 0 AND due_day BETWEEN 1 AND 28)
  );

CREATE INDEX IF NOT EXISTS idx_tenants_pending_allocation
  ON tenants(property_id) WHERE status = 'pending_allocation';
