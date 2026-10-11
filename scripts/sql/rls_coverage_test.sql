-- RLS Coverage and Ratchet Regression Suite
--
-- Run against a database with all migrations applied:
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f scripts/sql/rls_coverage_test.sql
--
-- Safe on dev/test: wrapped in transaction that terminates in ROLLBACK.
\set ON_ERROR_STOP on
\set QUIET on
BEGIN;

CREATE TEMP TABLE _rls_checks (label text);

-- 1. Verify Application and Maintenance Roles Configuration
DO $$
DECLARE
    app_bypass boolean;
    maint_bypass boolean;
BEGIN
    SELECT rolbypassrls INTO app_bypass FROM pg_roles WHERE rolname = 'pgapp_app';
    IF app_bypass IS NULL THEN
        RAISE EXCEPTION 'FAIL: Role pgapp_app does not exist';
    END IF;
    IF app_bypass = true THEN
        RAISE EXCEPTION 'FAIL: Role pgapp_app must have NOBYPASSRLS';
    END IF;

    SELECT rolbypassrls INTO maint_bypass FROM pg_roles WHERE rolname = 'pgapp_maint';
    IF maint_bypass IS NULL THEN
        RAISE EXCEPTION 'FAIL: Role pgapp_maint does not exist';
    END IF;
    IF maint_bypass = true THEN
        RAISE EXCEPTION 'FAIL: Role pgapp_maint must have NOBYPASSRLS';
    END IF;

    INSERT INTO _rls_checks VALUES ('roles_nobypassrls_verified');
END $$;

-- 2. Verify 13 Core Tables Have RLS Enabled and Forced
DO $$
DECLARE
    tbl text;
    core_tables text[] := ARRAY[
        'bank_transactions',
        'deposit_settlements',
        'dues',
        'events',
        'expenses',
        'financial_journal_entries',
        'gateway_refunds',
        'gateway_settlements',
        'payment_reports',
        'payments',
        'payout_batches',
        'payout_payees',
        'tenants'
    ];
    r_sec boolean;
    r_force boolean;
BEGIN
    FOREACH tbl IN ARRAY core_tables LOOP
        SELECT relrowsecurity, relforcerowsecurity INTO r_sec, r_force
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public' AND c.relname = tbl;

        IF r_sec IS NOT TRUE THEN
            RAISE EXCEPTION 'FAIL: Core table % does not have RLS enabled', tbl;
        END IF;
        IF r_force IS NOT TRUE THEN
            RAISE EXCEPTION 'FAIL: Core table % does not have RLS forced', tbl;
        END IF;
    END LOOP;

    INSERT INTO _rls_checks VALUES ('core_13_tables_rls_forced');
END $$;

-- 3. RLS Ratchet: Verify No Unregistered Property-Scoped Tables Lack RLS
DO $$
DECLARE
    r RECORD;
    debt_register text[] := ARRAY[
        'approval_policies',
        'approval_requests',
        'attendance_records',
        'bank_accounts',
        'budgets',
        'capital_transactions',
        'daily_financial_rollups',
        'daily_settlement_balance_runs',
        'daily_settlement_balances',
        'expense_import_suggestions',
        'expense_payments',
        'financial_categories',
        'financial_corrections',
        'floors',
        'forecast_snapshots',
        'hazards',
        'import_logs',
        'inspections',
        'join_requests',
        'kpi_snapshots',
        'leakage_events',
        'leave_policies',
        'ledger_control_overrides',
        'ledger_outbox_events',
        'manager_advances',
        'manager_reimbursements',
        'meal_prep_actuals',
        'meal_rsvps',
        'menu_polls',
        'meter_readings',
        'notifications',
        'outbox_events',
        'payout_records',
        'period_tie_outs',
        'point_rules',
        'points_ledger',
        'property_calendar_tokens',
        'property_finance_settings',
        'property_gamification_settings',
        'property_settings',
        'recommendations',
        'redemptions',
        'referrals',
        'reward_liability_transactions',
        'rewards_catalog',
        'roi_snapshots',
        'rooms',
        'search_documents',
        'staff_profiles',
        'tenant_departures',
        'tenant_streaks',
        'unmatched_gateway_receipts',
        'users',
        'vendor_inspections',
        'violations',
        'wage_calculations'
    ];
    violation_count integer := 0;
    unregistered text := '';
BEGIN
    FOR r IN (
        SELECT c.relname
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE n.nspname = 'public'
          AND c.relkind = 'r'
          AND c.relrowsecurity = false
          AND EXISTS (
              SELECT 1 FROM pg_attribute a
              WHERE a.attrelid = c.oid
                AND a.attname = 'property_id'
                AND NOT a.attisdropped
          )
    ) LOOP
        IF NOT (r.relname = ANY(debt_register)) THEN
            violation_count := violation_count + 1;
            unregistered := unregistered || ' ' || r.relname;
        END IF;
    END LOOP;

    IF violation_count > 0 THEN
        RAISE EXCEPTION 'FAIL: RLS Ratchet violation! Found % unregistered property-scoped table(s) without RLS:%', violation_count, unregistered;
    END IF;

    INSERT INTO _rls_checks VALUES ('rls_ratchet_debt_register_verified');
END $$;

-- 4. Verify Append-Only Tables Deny UPDATE and DELETE to pgapp_app
DO $$
DECLARE
    tbl text;
    append_only_tables text[] := ARRAY[
        'financial_journal_entries',
        'capital_transactions',
        'expense_payments',
        'payout_records'
    ];
BEGIN
    FOREACH tbl IN ARRAY append_only_tables LOOP
        IF has_table_privilege('pgapp_app', tbl, 'UPDATE') THEN
            RAISE EXCEPTION 'FAIL: pgapp_app has forbidden UPDATE privilege on %', tbl;
        END IF;
        IF has_table_privilege('pgapp_app', tbl, 'DELETE') THEN
            RAISE EXCEPTION 'FAIL: pgapp_app has forbidden DELETE privilege on %', tbl;
        END IF;
        IF NOT has_table_privilege('pgapp_app', tbl, 'INSERT') THEN
            RAISE EXCEPTION 'FAIL: pgapp_app missing required INSERT privilege on %', tbl;
        END IF;
        IF NOT has_table_privilege('pgapp_app', tbl, 'SELECT') THEN
            RAISE EXCEPTION 'FAIL: pgapp_app missing required SELECT privilege on %', tbl;
        END IF;
    END LOOP;

    INSERT INTO _rls_checks VALUES ('append_only_privileges_verified');
END $$;

-- 5. Verify TRUNCATE Denied on All Tables to pgapp_app
DO $$
DECLARE
    r RECORD;
BEGIN
    FOR r IN (
        SELECT tablename FROM pg_tables WHERE schemaname = 'public'
    ) LOOP
        IF has_table_privilege('pgapp_app', 'public.' || quote_ident(r.tablename), 'TRUNCATE') THEN
            RAISE EXCEPTION 'FAIL: pgapp_app has TRUNCATE privilege on table %', r.tablename;
        END IF;
    END LOOP;

    INSERT INTO _rls_checks VALUES ('truncate_revoked_verified');
END $$;

-- 6. Verify budgets Has UPDATE (Needed for UpsertBudget)
DO $$
BEGIN
    IF NOT has_table_privilege('pgapp_app', 'budgets', 'UPDATE') THEN
        RAISE EXCEPTION 'FAIL: pgapp_app missing required UPDATE privilege on budgets for upsert';
    END IF;
    INSERT INTO _rls_checks VALUES ('budgets_update_verified');
END $$;

SELECT count(*) AS passed_checks FROM _rls_checks;

ROLLBACK;
