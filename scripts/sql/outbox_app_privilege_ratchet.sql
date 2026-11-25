-- Run after migrations. Fails if pgapp_app can mutate the worker-owned outbox.
DO $$
BEGIN
    IF has_table_privilege('pgapp_app', 'outbox_events', 'UPDATE') THEN
        RAISE EXCEPTION 'least-privilege regression: pgapp_app has UPDATE on outbox_events';
    END IF;
    IF has_table_privilege('pgapp_app', 'outbox_events', 'DELETE') THEN
        RAISE EXCEPTION 'least-privilege regression: pgapp_app has DELETE on outbox_events';
    END IF;
    IF has_table_privilege('pgapp_app', 'outbox_events', 'TRUNCATE') THEN
        RAISE EXCEPTION 'least-privilege regression: pgapp_app has TRUNCATE on outbox_events';
    END IF;
    IF has_table_privilege('pgapp_app', 'outbox_events', 'REFERENCES') THEN
        RAISE EXCEPTION 'least-privilege regression: pgapp_app has REFERENCES on outbox_events';
    END IF;
    IF has_table_privilege('pgapp_app', 'outbox_events', 'TRIGGER') THEN
        RAISE EXCEPTION 'least-privilege regression: pgapp_app has TRIGGER on outbox_events';
    END IF;
    IF NOT has_table_privilege('pgapp_app', 'outbox_events', 'INSERT') THEN
        RAISE EXCEPTION 'outbox application insert capability was removed';
    END IF;
    IF NOT has_table_privilege('pgapp_app', 'outbox_events', 'SELECT') THEN
        RAISE EXCEPTION 'outbox application RETURNING capability was removed';
    END IF;
    IF NOT has_sequence_privilege('pgapp_app', 'outbox_events_id_seq', 'USAGE') THEN
        RAISE EXCEPTION 'outbox application sequence USAGE capability was removed';
    END IF;
    IF has_sequence_privilege('pgapp_app', 'outbox_events_id_seq', 'SELECT') THEN
        RAISE EXCEPTION 'least-privilege regression: pgapp_app can read outbox sequence state';
    END IF;
    IF has_sequence_privilege('pgapp_app', 'outbox_events_id_seq', 'UPDATE') THEN
        RAISE EXCEPTION 'least-privilege regression: pgapp_app can change outbox sequence state';
    END IF;
    IF EXISTS (
        SELECT 1
        FROM pg_policies
        WHERE schemaname = current_schema()
          AND tablename = 'outbox_events'
          AND 'pgapp_app' = ANY(roles)
          AND cmd IN ('ALL', 'UPDATE', 'DELETE')
    ) THEN
        RAISE EXCEPTION 'least-privilege regression: pgapp_app has a mutating RLS policy on outbox_events';
    END IF;
END;
$$;
