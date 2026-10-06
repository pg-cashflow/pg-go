-- Migration 057: Role-targeted Row Level Security, financial table coverage,
-- composite keyset index on dues, and ledger-adjacent foreign key restrict.
--
-- Addresses:
-- 1. P0: Role-targeted tenant isolation via pgapp_app and pgapp_maint roles.
-- 2. P1: Elimination of OR-based GUC bypass in RLS policies (restoring index scan efficiency).
-- 3. P1: Complete RLS coverage across financial tables (financial_journal_entries, bank_transactions,
--        gateway_settlements, gateway_refunds, payout_batches, events, payment_reports).
-- 4. P1: Addition of composite index idx_dues_prop_due_date_id for keyset dues pagination.
-- 5. Foreign key RESTRICT on ledger-adjacent tables (preventing accidental cascade deletion).

-- -----------------------------------------------------------------------------
-- 1. Create Dedicated Application and Maintenance Roles
-- -----------------------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'pgapp_app') THEN
        CREATE ROLE pgapp_app WITH NOSUPERUSER NOBYPASSRLS;
    END IF;
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'pgapp_maint') THEN
        CREATE ROLE pgapp_maint WITH NOSUPERUSER NOBYPASSRLS;
    END IF;
END $$;

GRANT USAGE ON SCHEMA public TO pgapp_app, pgapp_maint;
GRANT ALL ON ALL TABLES IN SCHEMA public TO pgapp_app, pgapp_maint;
GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO pgapp_app, pgapp_maint;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON TABLES TO pgapp_app, pgapp_maint;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT ALL ON SEQUENCES TO pgapp_app, pgapp_maint;

-- -----------------------------------------------------------------------------
-- 2. Role-Targeted Policies on Core Tables (Eliminating OR GUC branch)
-- -----------------------------------------------------------------------------

-- Helper macro applied to all 13 property-scoped tables
DO $$
DECLARE
    tbl TEXT;
    tables TEXT[] := ARRAY[
        'tenants',
        'dues',
        'payments',
        'expenses',
        'payout_payees',
        'deposit_settlements',
        'financial_journal_entries',
        'bank_transactions',
        'gateway_settlements',
        'gateway_refunds',
        'payout_batches',
        'events',
        'payment_reports'
    ];
BEGIN
    FOREACH tbl IN ARRAY tables LOOP
        -- Ensure RLS is enabled and forced
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY;', tbl);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY;', tbl);

        -- Drop legacy or previous policies
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', tbl || '_property_isolation', tbl);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', 'due_property_isolation', tbl);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', 'tenant_property_isolation', tbl);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', 'payment_property_isolation', tbl);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', 'expense_property_isolation', tbl);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', 'payout_payee_property_isolation', tbl);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', 'deposit_settlements_property_isolation', tbl);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', tbl || '_app', tbl);
        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', tbl || '_maint', tbl);

        -- Create index-friendly fail-closed policy for application and public access
        EXECUTE format('
            CREATE POLICY %I ON %I
            FOR ALL
            USING (
                property_id = (
                    CASE 
                        WHEN NULLIF(current_setting(''app.current_property_id'', true), '''') IS NOT NULL 
                        THEN current_setting(''app.current_property_id'', true)::UUID 
                        ELSE NULL 
                    END
                )
            )
            WITH CHECK (
                property_id = (
                    CASE 
                        WHEN NULLIF(current_setting(''app.current_property_id'', true), '''') IS NOT NULL 
                        THEN current_setting(''app.current_property_id'', true)::UUID 
                        ELSE NULL 
                    END
                )
            );
        ', tbl || '_app', tbl);

        -- Create maintenance policy targeted exclusively to pgapp_maint role
        EXECUTE format('
            CREATE POLICY %I ON %I
            FOR ALL
            TO pgapp_maint
            USING (true)
            WITH CHECK (true);
        ', tbl || '_maint', tbl);
    END LOOP;
END $$;

-- -----------------------------------------------------------------------------
-- 3. High-Performance Composite Keyset Index on Dues
-- -----------------------------------------------------------------------------
CREATE INDEX IF NOT EXISTS idx_dues_prop_due_date_id
    ON dues (property_id, due_date DESC, id DESC);

-- -----------------------------------------------------------------------------
-- 4. Foreign Key RESTRICT on Ledger-Adjacent Tables
-- -----------------------------------------------------------------------------
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN (
        SELECT conname, relname
        FROM pg_constraint c
        JOIN pg_class cl ON cl.oid = c.conrelid
        WHERE c.contype = 'f'
          AND cl.relname IN ('expenses', 'bank_transactions', 'deposit_settlements', 'payout_batches', 'ledger_outbox_events')
          AND c.confrelid = 'properties'::regclass
    ) LOOP
        EXECUTE 'ALTER TABLE ' || quote_ident(r.relname) || ' DROP CONSTRAINT ' || quote_ident(r.conname);
        EXECUTE 'ALTER TABLE ' || quote_ident(r.relname) || ' ADD CONSTRAINT ' || quote_ident(r.conname) || ' FOREIGN KEY (property_id) REFERENCES properties(id) ON DELETE RESTRICT';
    END LOOP;
END $$;
