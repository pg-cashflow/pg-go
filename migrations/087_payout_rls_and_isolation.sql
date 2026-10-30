SET lock_timeout = '5s';

-- Migration 087: Enable and force Row Level Security on payout_records, payout_record_items, and payout_items.
-- Standard: ASD-STE100
-- Method: Empirical First-Principles Verification
-- Reference: RLS_D1_STATUS.md (Ticket R9-F4), ADR-018, ADR-022, ADR-029

-- 1. payout_records
ALTER TABLE payout_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE payout_records FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS payout_records_app ON payout_records;
CREATE POLICY payout_records_app ON payout_records
    FOR ALL TO pgapp_app
    USING (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
    WITH CHECK (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid);

DROP POLICY IF EXISTS payout_records_maint ON payout_records;
CREATE POLICY payout_records_maint ON payout_records
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

GRANT SELECT, INSERT ON payout_records TO pgapp_app, pgapp_maint, pgapp_worker;

-- 2. payout_record_items
ALTER TABLE payout_record_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE payout_record_items FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS payout_record_items_app ON payout_record_items;
CREATE POLICY payout_record_items_app ON payout_record_items
    FOR ALL TO pgapp_app
    USING (
        EXISTS (
            SELECT 1 FROM payout_records pr
            WHERE pr.id = payout_record_items.payout_record_id
              AND pr.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM payout_records pr
            WHERE pr.id = payout_record_items.payout_record_id
              AND pr.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    );

DROP POLICY IF EXISTS payout_record_items_maint ON payout_record_items;
CREATE POLICY payout_record_items_maint ON payout_record_items
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

GRANT SELECT, INSERT ON payout_record_items TO pgapp_app, pgapp_maint, pgapp_worker;

-- 3. payout_items
ALTER TABLE payout_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE payout_items FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS payout_items_app ON payout_items;
CREATE POLICY payout_items_app ON payout_items
    FOR ALL TO pgapp_app
    USING (
        EXISTS (
            SELECT 1 FROM payout_payees pp
            WHERE pp.id = payout_items.payee_id
              AND pp.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM payout_payees pp
            WHERE pp.id = payout_items.payee_id
              AND pp.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    );

DROP POLICY IF EXISTS payout_items_maint ON payout_items;
CREATE POLICY payout_items_maint ON payout_items
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE ON payout_items TO pgapp_app, pgapp_maint, pgapp_worker;
