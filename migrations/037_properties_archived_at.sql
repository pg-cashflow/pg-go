-- Migration 037: soft-archive for properties.
-- Properties that have recorded daily_settlement_balance_runs cannot be hard-deleted (035 makes that
-- audit table append-only, and the ON DELETE CASCADE from properties is rejected by the trigger).
-- Archiving is the supported way to retire a property.
ALTER TABLE properties ADD COLUMN IF NOT EXISTS archived_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_properties_active ON properties (created_at) WHERE archived_at IS NULL;
