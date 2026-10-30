SET lock_timeout = '5s';

-- Migration 089: Create dedicated payout_approval_snapshots table with full RLS
-- Implements immutable destination binding (A2.2) to prevent payee instrument tampering after approval.

CREATE TABLE IF NOT EXISTS payout_approval_snapshots (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    batch_id UUID NOT NULL REFERENCES payout_batches(id) ON DELETE RESTRICT,
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE RESTRICT,
    schema_version INT NOT NULL DEFAULT 1,
    approved_by UUID NOT NULL REFERENCES users(id),
    approved_at TIMESTAMPTZ NOT NULL,
    total_amount_paise BIGINT NOT NULL,
    item_count INT NOT NULL,
    snapshot_data JSONB NOT NULL,
    signature TEXT NOT NULL,
    key_id TEXT NOT NULL DEFAULT 'v1',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_payout_approval_snapshots_batch UNIQUE (batch_id)
);

CREATE INDEX IF NOT EXISTS idx_payout_approval_snapshots_prop ON payout_approval_snapshots (property_id);

-- Enable and force Row Level Security
ALTER TABLE payout_approval_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE payout_approval_snapshots FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS payout_approval_snapshots_app ON payout_approval_snapshots;
CREATE POLICY payout_approval_snapshots_app ON payout_approval_snapshots
    FOR ALL TO pgapp_app
    USING (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
    WITH CHECK (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid);

DROP POLICY IF EXISTS payout_approval_snapshots_maint ON payout_approval_snapshots;
CREATE POLICY payout_approval_snapshots_maint ON payout_approval_snapshots
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

GRANT SELECT, INSERT ON payout_approval_snapshots TO pgapp_app, pgapp_maint, pgapp_worker;
