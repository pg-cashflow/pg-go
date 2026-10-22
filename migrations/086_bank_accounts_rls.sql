-- Migration 086: Enable and force Row Level Security on bank_accounts.
-- Standard: ASD-STE100
-- Method: Empirical First-Principles Verification
-- Reference: RLS_D1_STATUS.md (Ticket R9-F1), ADR-018, ADR-029

ALTER TABLE bank_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE bank_accounts FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS bank_accounts_app ON bank_accounts;
CREATE POLICY bank_accounts_app ON bank_accounts
    FOR ALL TO pgapp_app
    USING (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
    WITH CHECK (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid);

DROP POLICY IF EXISTS bank_accounts_maint ON bank_accounts;
CREATE POLICY bank_accounts_maint ON bank_accounts
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE, DELETE ON bank_accounts TO pgapp_app, pgapp_maint, pgapp_worker;
