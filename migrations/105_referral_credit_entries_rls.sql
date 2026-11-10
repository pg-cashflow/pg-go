SET lock_timeout = '5s';

-- Migration 105: Enable and force Row Level Security on referral_credit_entries.
-- Standard: ASD-STE100
-- Reference: Migration 074/094 pattern, ADR-018, ADR-029

ALTER TABLE referral_credit_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE referral_credit_entries FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS referral_credit_entries_app ON referral_credit_entries;
CREATE POLICY referral_credit_entries_app ON referral_credit_entries
    FOR ALL TO pgapp_app
    USING (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
    WITH CHECK (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid);

DROP POLICY IF EXISTS referral_credit_entries_maint ON referral_credit_entries;
CREATE POLICY referral_credit_entries_maint ON referral_credit_entries
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE, DELETE ON referral_credit_entries TO pgapp_app, pgapp_maint, pgapp_worker;

