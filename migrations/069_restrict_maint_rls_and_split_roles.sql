-- Migration 069: Restrict Maintenance Role RLS Policies and Split Worker Privileges.
--
-- Follows 057 and 058.
-- Standard: ASD-STE100
-- Method: Empirical First-Principles Verification
--
-- Addresses:
-- 1. P0: Restrict permissive USING (true) on pgapp_maint for the 13 core tables.
--    - Financial tables (financial_journal_entries, expenses, payout_payees,
--      deposit_settlements, bank_transactions, payout_batches) enforce strict fail-closed
--      scoping to NULLIF(current_setting('app.current_property_id', true), '')::UUID.
--    - Operational tables (events, payments, dues, tenants, gateway_settlements,
--      gateway_refunds, payment_reports) enforce property scoping when app.current_property_id
--      is active, permitting unscoped access only when the GUC is unset.
-- 2. P0: Grant pgapp_app to pgapp_maint and pgapp_worker, enabling safe in-transaction
--    role switching (SET LOCAL ROLE pgapp_app) in WithinTx.
-- 3. P1: Create dedicated pgapp_worker role for background tasks and webhooks.

-- 1. Create Dedicated Worker Role (pgapp_worker)
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'pgapp_worker') THEN
        CREATE ROLE pgapp_worker WITH NOSUPERUSER NOBYPASSRLS;
    END IF;
    BEGIN
        ALTER ROLE pgapp_worker LOGIN NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB;
    EXCEPTION WHEN insufficient_privilege THEN
        NULL;
    END;
END $$;

GRANT USAGE ON SCHEMA public TO pgapp_worker;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO pgapp_worker;
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO pgapp_worker;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO pgapp_worker;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO pgapp_worker;

REVOKE ALL ON schema_migrations FROM pgapp_worker;
REVOKE UPDATE, DELETE ON financial_journal_entries, capital_transactions, expense_payments, payout_records, payout_record_items FROM pgapp_worker;

-- 2. Enable In-Transaction Role Switching from Maintenance / Worker to Application Role
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_app') AND
       EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_maint') THEN
        BEGIN
            GRANT pgapp_app TO pgapp_maint;
        EXCEPTION WHEN insufficient_privilege THEN
            NULL;
        END;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_app') AND
       EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_worker') THEN
        BEGIN
            GRANT pgapp_app TO pgapp_worker;
        EXCEPTION WHEN insufficient_privilege THEN
            NULL;
        END;
    END IF;
END $$;

-- 3. Restrict Maintenance & Worker Policies on Financial Tables (Strict Fail-Closed)
DO $$
DECLARE
    tbl TEXT;
    financial_tables TEXT[] := ARRAY[
        'financial_journal_entries',
        'expenses',
        'payout_payees',
        'deposit_settlements',
        'bank_transactions',
        'payout_batches'
    ];
BEGIN
    FOREACH tbl IN ARRAY financial_tables LOOP
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', tbl || '_maint', tbl);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', tbl || '_worker', tbl);

        -- Strict fail-closed policy: requires app.current_property_id
        EXECUTE format($p$
            CREATE POLICY %I ON %I FOR ALL TO pgapp_maint, pgapp_worker
            USING (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
            WITH CHECK (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid);
        $p$, tbl || '_maint', tbl);
    END LOOP;
END $$;

-- 4. Restrict Maintenance & Worker Policies on Operational Tables (Scoped When GUC Active)
DO $$
DECLARE
    tbl TEXT;
    operational_tables TEXT[] := ARRAY[
        'events',
        'payments',
        'dues',
        'tenants',
        'gateway_settlements',
        'gateway_refunds',
        'payment_reports'
    ];
BEGIN
    FOREACH tbl IN ARRAY operational_tables LOOP
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', tbl || '_maint', tbl);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', tbl || '_worker', tbl);

        -- Scoped when app.current_property_id is set; permits unscoped when GUC is unset
        EXECUTE format($p$
            CREATE POLICY %I ON %I FOR ALL TO pgapp_maint, pgapp_worker
            USING (
                NULLIF(current_setting('app.current_property_id', true), '') IS NULL
                OR property_id = current_setting('app.current_property_id', true)::uuid
            )
            WITH CHECK (
                NULLIF(current_setting('app.current_property_id', true), '') IS NULL
                OR property_id = current_setting('app.current_property_id', true)::uuid
            );
        $p$, tbl || '_maint', tbl);
    END LOOP;
END $$;
