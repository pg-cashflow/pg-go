-- Migration 058: privilege hardening, role-targeted policies, redundant index cleanup,
-- validation of NOT VALID constraints.
--
-- Follows 057. Tested on PostgreSQL 16.15 with all 57 prior migrations applied.
-- Idempotent: safe to re-run.
--
-- IMPORTANT: this migration only fixes the DATABASE side. The application must also
-- stop using `SET LOCAL ROLE` and connect with two dedicated login roles (see plan, Phase 1).
-- Set real passwords out of band, e.g.:  ALTER ROLE pgapp_app PASSWORD '...';

-- 1. Dedicated LOGIN roles. The app connects AS these roles, so no SET ROLE is needed
--    (SET ROLE fails for a plain login role that is not a member, and is unsafe under
--    PgBouncer transaction pooling when used at session level).
ALTER ROLE pgapp_app   LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB;
ALTER ROLE pgapp_maint LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB;

-- 2. Least privilege. 057 granted ALL, which includes TRUNCATE, TRIGGER and REFERENCES.
--    TRUNCATE ... CASCADE bypasses row-level append-only triggers and wiped the journal
--    plus gateway_settlements and bank_transactions in testing.
REVOKE ALL ON ALL TABLES    IN SCHEMA public FROM pgapp_app, pgapp_maint;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO pgapp_app, pgapp_maint;
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO pgapp_app, pgapp_maint;

ALTER DEFAULT PRIVILEGES IN SCHEMA public REVOKE ALL ON TABLES FROM pgapp_app, pgapp_maint;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO pgapp_app, pgapp_maint;

-- Runtime roles never edit migration bookkeeping.
REVOKE ALL ON schema_migrations FROM pgapp_app, pgapp_maint;

-- The journal is append-only: no UPDATE/DELETE for runtime roles (triggers remain as a second layer).
REVOKE UPDATE, DELETE ON financial_journal_entries FROM pgapp_app, pgapp_maint;

-- 3. Policies: scope the property policy to the app role only, so the maintenance role
--    has exactly one applicable policy (USING true) and no OR of two policies.
DO $$
DECLARE
    tbl TEXT;
    tables TEXT[] := ARRAY[
        'tenants','dues','payments','expenses','payout_payees','deposit_settlements',
        'financial_journal_entries','bank_transactions','gateway_settlements',
        'gateway_refunds','payout_batches','events','payment_reports'
    ];
BEGIN
    FOREACH tbl IN ARRAY tables LOOP
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I', tbl || '_app', tbl);
        EXECUTE format($p$
            CREATE POLICY %I ON %I FOR ALL TO pgapp_app
            USING (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
            WITH CHECK (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
        $p$, tbl || '_app', tbl);
    END LOOP;
END $$;

-- 4. Redundant indexes (verified: no Go or SQL reference to these names).
--    idx_dues_prop_due_date (property_id, due_date) is superseded by 057's
--    idx_dues_prop_due_date_id (property_id, due_date DESC, id DESC).
DROP INDEX IF EXISTS idx_dues_prop_due_date;
--    These two duplicate the UNIQUE indexes on the same columns exactly.
DROP INDEX IF EXISTS idx_daily_settlement_balances_prop_date;
DROP INDEX IF EXISTS idx_daily_financial_rollups_prop_date;

-- 5. Validate constraints that were added NOT VALID and never validated
--    (020, 021, 051). Fails loudly if any existing row violates one: fix the data, re-run.
DO $$
DECLARE r RECORD;
BEGIN
    FOR r IN SELECT conrelid::regclass AS tbl, conname FROM pg_constraint
             WHERE contype = 'c' AND NOT convalidated
    LOOP
        EXECUTE format('ALTER TABLE %s VALIDATE CONSTRAINT %I', r.tbl, r.conname);
    END LOOP;
END $$;
