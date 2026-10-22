-- Migration 085: Enable and force Row Level Security on property_calendar_tokens.
-- Standard: ASD-STE100
-- Method: Empirical First-Principles Verification
-- Reference: RLS_D1_STATUS.md (Ticket R9-F6), ADR-018, ADR-029

ALTER TABLE property_calendar_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE property_calendar_tokens FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS property_calendar_tokens_app ON property_calendar_tokens;
CREATE POLICY property_calendar_tokens_app ON property_calendar_tokens
    FOR ALL TO pgapp_app
    USING (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
    WITH CHECK (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid);

DROP POLICY IF EXISTS property_calendar_tokens_maint ON property_calendar_tokens;
CREATE POLICY property_calendar_tokens_maint ON property_calendar_tokens
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE, DELETE ON property_calendar_tokens TO pgapp_app, pgapp_maint, pgapp_worker;
