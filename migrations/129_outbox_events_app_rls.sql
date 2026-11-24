SET lock_timeout = '5s';

-- Migration 129: Reconcile outbox_events Application RLS Policy
-- Standard: ASD-STE100
-- Purpose:
--   1. Allow pgapp_app to insert and read outbox_events strictly for the current property.
--   2. Maintain worker and maintenance access for cluster background dispatchers.

DROP POLICY IF EXISTS outbox_events_app ON outbox_events;
CREATE POLICY outbox_events_app ON outbox_events
    FOR ALL TO pgapp_app
    USING (
        property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
    )
    WITH CHECK (
        property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
    );

GRANT SELECT, INSERT, UPDATE, DELETE ON outbox_events TO pgapp_app;
