SET lock_timeout = '5s';

-- Migration 131: Restrict application access to the notification outbox.
--
-- Application transactions only INSERT outbox_events. Background dispatch,
-- retries, cleanup, and status changes use pgapp_maint / pgapp_worker.
-- Keep SELECT because INSERT ... RETURNING reads the generated id and timestamp.
-- Grant sequence USAGE only; application roles must not inspect or reset it.
-- The application role must not mutate or truncate this worker-owned queue.

DROP POLICY IF EXISTS outbox_events_app ON outbox_events;
DROP POLICY IF EXISTS outbox_events_app_select ON outbox_events;
DROP POLICY IF EXISTS outbox_events_app_insert ON outbox_events;

CREATE POLICY outbox_events_app_select ON outbox_events
    FOR SELECT TO pgapp_app
    USING (
        property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
    );

CREATE POLICY outbox_events_app_insert ON outbox_events
    FOR INSERT TO pgapp_app
    WITH CHECK (
        property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
    );

REVOKE ALL PRIVILEGES ON TABLE outbox_events FROM pgapp_app;
GRANT SELECT, INSERT ON TABLE outbox_events TO pgapp_app;
REVOKE ALL PRIVILEGES ON SEQUENCE outbox_events_id_seq FROM pgapp_app;
GRANT USAGE ON SEQUENCE outbox_events_id_seq TO pgapp_app;

DO $$
BEGIN
    IF has_table_privilege('pgapp_app', 'outbox_events', 'UPDATE') THEN
        RAISE EXCEPTION 'pgapp_app must not have UPDATE on outbox_events';
    END IF;
    IF has_table_privilege('pgapp_app', 'outbox_events', 'DELETE') THEN
        RAISE EXCEPTION 'pgapp_app must not have DELETE on outbox_events';
    END IF;
    IF has_table_privilege('pgapp_app', 'outbox_events', 'TRUNCATE') THEN
        RAISE EXCEPTION 'pgapp_app must not have TRUNCATE on outbox_events';
    END IF;
    IF has_table_privilege('pgapp_app', 'outbox_events', 'REFERENCES') THEN
        RAISE EXCEPTION 'pgapp_app must not have REFERENCES on outbox_events';
    END IF;
    IF has_table_privilege('pgapp_app', 'outbox_events', 'TRIGGER') THEN
        RAISE EXCEPTION 'pgapp_app must not have TRIGGER on outbox_events';
    END IF;
    IF NOT has_table_privilege('pgapp_app', 'outbox_events', 'INSERT') THEN
        RAISE EXCEPTION 'pgapp_app must retain INSERT on outbox_events';
    END IF;
    IF NOT has_table_privilege('pgapp_app', 'outbox_events', 'SELECT') THEN
        RAISE EXCEPTION 'pgapp_app must retain SELECT on outbox_events for RETURNING';
    END IF;
    IF NOT has_sequence_privilege('pgapp_app', 'outbox_events_id_seq', 'USAGE') THEN
        RAISE EXCEPTION 'pgapp_app must retain sequence USAGE for outbox inserts';
    END IF;
    IF has_sequence_privilege('pgapp_app', 'outbox_events_id_seq', 'SELECT') THEN
        RAISE EXCEPTION 'pgapp_app must not read outbox sequence state';
    END IF;
    IF has_sequence_privilege('pgapp_app', 'outbox_events_id_seq', 'UPDATE') THEN
        RAISE EXCEPTION 'pgapp_app must not change outbox sequence state';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM pg_policies
        WHERE schemaname = current_schema()
          AND tablename = 'outbox_events'
          AND 'pgapp_app' = ANY(roles)
          AND cmd IN ('ALL', 'UPDATE', 'DELETE')
    ) THEN
        RAISE EXCEPTION 'pgapp_app must not have a mutating RLS policy on outbox_events';
    END IF;
END;
$$;
