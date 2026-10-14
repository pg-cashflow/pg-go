-- Migration 070: Manager capability grants.
-- Standard: ASD-STE100
-- Method: Empirical First-Principles Verification
-- ADR-032: one row per grant. Revoke sets revoked_at. Do not delete the row.

CREATE TABLE IF NOT EXISTS manager_capabilities (
    property_id UUID NOT NULL REFERENCES properties(id),
    user_id UUID NOT NULL REFERENCES users(id),
    capability TEXT NOT NULL,
    granted_by UUID,
    granted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at TIMESTAMPTZ,
    PRIMARY KEY (property_id, user_id, capability),
    CONSTRAINT manager_capabilities_delegable CHECK (capability IN (
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
        'notifications.send'
    ))
);

ALTER TABLE manager_capabilities ENABLE ROW LEVEL SECURITY;
ALTER TABLE manager_capabilities FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS manager_capabilities_app ON manager_capabilities;
CREATE POLICY manager_capabilities_app ON manager_capabilities
    FOR ALL TO pgapp_app
    USING (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
    WITH CHECK (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid);

DROP POLICY IF EXISTS manager_capabilities_maint ON manager_capabilities;
CREATE POLICY manager_capabilities_maint ON manager_capabilities
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE ON manager_capabilities TO pgapp_app, pgapp_maint, pgapp_worker;
