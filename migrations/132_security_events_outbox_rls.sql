SET lock_timeout = '5s';

-- Migration 132: Enable FORCE RLS and least-privilege policies on security_events_outbox.
-- Standard: ASD-STE100
-- Purpose:
--   1. Enable and force Row Level Security on security_events_outbox.
--   2. Allow pgapp_app only property-scoped SELECT and INSERT, and sequence USAGE.
--   3. Preserve full queue access for pgapp_worker and pgapp_maint.

ALTER TABLE security_events_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE security_events_outbox FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS security_events_outbox_app ON security_events_outbox;
DROP POLICY IF EXISTS security_events_outbox_app_select ON security_events_outbox;
DROP POLICY IF EXISTS security_events_outbox_app_insert ON security_events_outbox;
DROP POLICY IF EXISTS security_events_outbox_worker ON security_events_outbox;

CREATE POLICY security_events_outbox_app_select ON security_events_outbox
    FOR SELECT TO pgapp_app
    USING (
        property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
    );

CREATE POLICY security_events_outbox_app_insert ON security_events_outbox
    FOR INSERT TO pgapp_app
    WITH CHECK (
        property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
    );

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_worker') THEN
        EXECUTE 'CREATE POLICY security_events_outbox_worker ON security_events_outbox FOR ALL TO pgapp_worker USING (true) WITH CHECK (true)';
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_maint') THEN
        EXECUTE 'DROP POLICY IF EXISTS security_events_outbox_maint ON security_events_outbox';
        EXECUTE 'CREATE POLICY security_events_outbox_maint ON security_events_outbox FOR ALL TO pgapp_maint USING (true) WITH CHECK (true)';
    END IF;
END $$;

REVOKE ALL PRIVILEGES ON TABLE security_events_outbox FROM pgapp_app;
GRANT SELECT, INSERT ON TABLE security_events_outbox TO pgapp_app;
REVOKE ALL PRIVILEGES ON SEQUENCE security_events_outbox_id_seq FROM pgapp_app;
GRANT USAGE ON SEQUENCE security_events_outbox_id_seq TO pgapp_app;
