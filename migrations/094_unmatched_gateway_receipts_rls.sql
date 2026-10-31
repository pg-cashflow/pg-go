SET lock_timeout = '5s';

-- Migration 094: Enable and force Row Level Security on unmatched_gateway_receipts.
-- Standard: ASD-STE100
-- Method: Empirical First-Principles Verification
-- Reference: Migration 086 pattern, ADR-018, ADR-029

ALTER TABLE unmatched_gateway_receipts ENABLE ROW LEVEL SECURITY;
ALTER TABLE unmatched_gateway_receipts FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS unmatched_gateway_receipts_app ON unmatched_gateway_receipts;
CREATE POLICY unmatched_gateway_receipts_app ON unmatched_gateway_receipts
    FOR ALL TO pgapp_app
    USING (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
    WITH CHECK (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid);

DROP POLICY IF EXISTS unmatched_gateway_receipts_maint ON unmatched_gateway_receipts;
CREATE POLICY unmatched_gateway_receipts_maint ON unmatched_gateway_receipts
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE, DELETE ON unmatched_gateway_receipts TO pgapp_app, pgapp_maint, pgapp_worker;
