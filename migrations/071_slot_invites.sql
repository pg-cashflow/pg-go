SET lock_timeout = '5s';

-- Migration 071: Slot invites, room occupancy reservations, onboarding settings.
-- ADR: docs/adr/033-slot-invites-and-occupancy.md

ALTER TABLE properties
  ADD COLUMN IF NOT EXISTS occupancy_revision BIGINT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS property_onboarding_settings (
    property_id UUID PRIMARY KEY REFERENCES properties(id) ON DELETE CASCADE,
    default_rent_paise BIGINT NOT NULL DEFAULT 0,
    default_deposit_paise BIGINT NOT NULL DEFAULT 0,
    default_due_day SMALLINT,
    auto_activate BOOLEAN NOT NULL DEFAULT FALSE,
    reserve_beds_for_open_invites BOOLEAN NOT NULL DEFAULT TRUE,
    max_extra_beds_per_room INT NOT NULL DEFAULT 0,
    slot_invite_default_ttl_days INT NOT NULL DEFAULT 14,
    waitlist_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    profile_schema_version INT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_onboarding_due_day CHECK (default_due_day IS NULL OR default_due_day BETWEEN 1 AND 28)
);

INSERT INTO property_onboarding_settings (property_id)
SELECT id FROM properties
ON CONFLICT (property_id) DO NOTHING;

CREATE TABLE IF NOT EXISTS room_occupancy (
    room_id UUID PRIMARY KEY REFERENCES rooms(id) ON DELETE CASCADE,
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    open_invite_reserved INT NOT NULL DEFAULT 0,
    over_capacity_allocated INT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_room_occupancy_nonneg CHECK (open_invite_reserved >= 0 AND over_capacity_allocated >= 0)
);

CREATE INDEX IF NOT EXISTS idx_room_occupancy_property ON room_occupancy(property_id);

CREATE TABLE IF NOT EXISTS slot_invites (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    floor_id UUID NOT NULL REFERENCES floors(id) ON DELETE CASCADE,
    room_id UUID NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    code VARCHAR(16) NOT NULL,
    rent_paise BIGINT NOT NULL,
    deposit_paise BIGINT NOT NULL DEFAULT 0,
    due_day SMALLINT NOT NULL,
    status TEXT NOT NULL DEFAULT 'open',
    terms_frozen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    profile_schema_version INT NOT NULL DEFAULT 1,
    allow_over_capacity BOOLEAN NOT NULL DEFAULT FALSE,
    expires_at TIMESTAMPTZ NOT NULL,
    consumed_at TIMESTAMPTZ,
    consumed_by_user_id UUID REFERENCES users(id),
    consumed_tenant_id UUID REFERENCES tenants(id),
    created_by_user_id UUID REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_slot_invites_code UNIQUE (code),
    CONSTRAINT chk_slot_invites_due_day CHECK (due_day BETWEEN 1 AND 28),
    CONSTRAINT chk_slot_invites_status CHECK (status IN ('open', 'consumed', 'revoked', 'expired')),
    CONSTRAINT chk_slot_invites_rent CHECK (rent_paise > 0)
);

CREATE INDEX IF NOT EXISTS idx_slot_invites_property_status ON slot_invites(property_id, status);
CREATE INDEX IF NOT EXISTS idx_slot_invites_room_open ON slot_invites(room_id) WHERE status = 'open';
CREATE INDEX IF NOT EXISTS idx_slot_invites_expires_open ON slot_invites(expires_at) WHERE status = 'open';

ALTER TABLE join_requests
  ADD COLUMN IF NOT EXISTS slot_invite_id UUID REFERENCES slot_invites(id);

CREATE TABLE IF NOT EXISTS slot_invite_audit (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    slot_invite_id UUID REFERENCES slot_invites(id) ON DELETE SET NULL,
    actor_user_id UUID REFERENCES users(id),
    action TEXT NOT NULL,
    reason TEXT,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_slot_invite_audit_property ON slot_invite_audit(property_id, created_at DESC);

ALTER TABLE slot_invites ENABLE ROW LEVEL SECURITY;
ALTER TABLE slot_invites FORCE ROW LEVEL SECURITY;
ALTER TABLE room_occupancy ENABLE ROW LEVEL SECURITY;
ALTER TABLE room_occupancy FORCE ROW LEVEL SECURITY;
ALTER TABLE property_onboarding_settings ENABLE ROW LEVEL SECURITY;
ALTER TABLE property_onboarding_settings FORCE ROW LEVEL SECURITY;
ALTER TABLE slot_invite_audit ENABLE ROW LEVEL SECURITY;
ALTER TABLE slot_invite_audit FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS slot_invites_app ON slot_invites;
CREATE POLICY slot_invites_app ON slot_invites
    FOR ALL TO pgapp_app
    USING (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
    WITH CHECK (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid);

DROP POLICY IF EXISTS slot_invites_maint ON slot_invites;
CREATE POLICY slot_invites_maint ON slot_invites
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true) WITH CHECK (true);

DROP POLICY IF EXISTS room_occupancy_app ON room_occupancy;
CREATE POLICY room_occupancy_app ON room_occupancy
    FOR ALL TO pgapp_app
    USING (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
    WITH CHECK (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid);

DROP POLICY IF EXISTS room_occupancy_maint ON room_occupancy;
CREATE POLICY room_occupancy_maint ON room_occupancy
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true) WITH CHECK (true);

DROP POLICY IF EXISTS property_onboarding_settings_app ON property_onboarding_settings;
CREATE POLICY property_onboarding_settings_app ON property_onboarding_settings
    FOR ALL TO pgapp_app
    USING (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
    WITH CHECK (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid);

DROP POLICY IF EXISTS property_onboarding_settings_maint ON property_onboarding_settings;
CREATE POLICY property_onboarding_settings_maint ON property_onboarding_settings
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true) WITH CHECK (true);

DROP POLICY IF EXISTS slot_invite_audit_app ON slot_invite_audit;
CREATE POLICY slot_invite_audit_app ON slot_invite_audit
    FOR ALL TO pgapp_app
    USING (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
    WITH CHECK (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid);

DROP POLICY IF EXISTS slot_invite_audit_maint ON slot_invite_audit;
CREATE POLICY slot_invite_audit_maint ON slot_invite_audit
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true) WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE ON slot_invites, room_occupancy, property_onboarding_settings, slot_invite_audit TO pgapp_app, pgapp_maint, pgapp_worker;

ALTER TABLE manager_capabilities DROP CONSTRAINT IF EXISTS manager_capabilities_delegable;
ALTER TABLE manager_capabilities ADD CONSTRAINT manager_capabilities_delegable CHECK (capability IN (
    'tenants.view',
    'dues.view',
    'dues.mark_cash_paid',
    'expenses.post',
    'expenses.void',
    'attendance.mark',
    'inspections.write',
    'meters.write',
    'kitchen.headcount',
    'complaints.respond',
    'notifications.send',
    'slot_invites.create',
    'slot_invites.allow_over_capacity'
));
