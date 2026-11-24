SET lock_timeout = '5s';

-- Migration 127: Complete Multi-Tenant Row Level Security Coverage
-- Standard: ASD-STE100
-- Purpose:
--   Enforce strict property isolation across all financial, operational, and KYC tables
--   that previously lacked database-level RLS policies.
--   Policies fail closed for pgapp_app when app.current_property_id is unset or empty,
--   while permitting unrestricted maintenance access for pgapp_maint and pgapp_worker.

-- ============================================================================
-- 1. Direct Property-Scoped Tables
-- ============================================================================
DO $$
DECLARE
    tbl TEXT;
    tables TEXT[] := ARRAY[
        'capital_transactions',
        'expense_payments',
        'manager_advances',
        'manager_reimbursements',
        'budgets',
        'property_finance_settings',
        'approval_policies',
        'financial_corrections',
        'daily_financial_rollups',
        'daily_settlement_balances',
        'daily_settlement_balance_runs',
        'ledger_control_overrides',
        'tenant_departures'
    ];
BEGIN
    FOREACH tbl IN ARRAY tables LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY;', tbl);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY;', tbl);

        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', tbl || '_app', tbl);
        EXECUTE format('
            CREATE POLICY %I ON %I
            FOR ALL TO pgapp_app
            USING (property_id = NULLIF(current_setting(''app.current_property_id'', true), '''')::uuid)
            WITH CHECK (property_id = NULLIF(current_setting(''app.current_property_id'', true), '''')::uuid);
        ', tbl || '_app', tbl);

        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', tbl || '_maint', tbl);
        EXECUTE format('
            CREATE POLICY %I ON %I
            FOR ALL TO pgapp_maint, pgapp_worker
            USING (true)
            WITH CHECK (true);
        ', tbl || '_maint', tbl);
    END LOOP;
END $$;

-- ============================================================================
-- 2. Parent-Linked Tables (departure_deductions, departure_due_adjustments)
-- ============================================================================

-- departure_deductions
ALTER TABLE departure_deductions ENABLE ROW LEVEL SECURITY;
ALTER TABLE departure_deductions FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS departure_deductions_app ON departure_deductions;
CREATE POLICY departure_deductions_app ON departure_deductions
    FOR ALL TO pgapp_app
    USING (
        EXISTS (
            SELECT 1 FROM tenant_departures td
            WHERE td.id = departure_deductions.departure_id
              AND td.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM tenant_departures td
            WHERE td.id = departure_deductions.departure_id
              AND td.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    );

DROP POLICY IF EXISTS departure_deductions_maint ON departure_deductions;
CREATE POLICY departure_deductions_maint ON departure_deductions
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

-- departure_due_adjustments
ALTER TABLE departure_due_adjustments ENABLE ROW LEVEL SECURITY;
ALTER TABLE departure_due_adjustments FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS departure_due_adjustments_app ON departure_due_adjustments;
CREATE POLICY departure_due_adjustments_app ON departure_due_adjustments
    FOR ALL TO pgapp_app
    USING (
        EXISTS (
            SELECT 1 FROM tenant_departures td
            WHERE td.id = departure_due_adjustments.departure_id
              AND td.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM tenant_departures td
            WHERE td.id = departure_due_adjustments.departure_id
              AND td.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    );

DROP POLICY IF EXISTS departure_due_adjustments_maint ON departure_due_adjustments;
CREATE POLICY departure_due_adjustments_maint ON departure_due_adjustments
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

-- ============================================================================
-- 3. KYC Tables Isolation (kyc_verification, kyc_consent, kyc_audit_log, kyc_webhook_inbox)
-- ============================================================================

-- kyc_verification
ALTER TABLE kyc_verification ENABLE ROW LEVEL SECURITY;
ALTER TABLE kyc_verification FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS kyc_verification_app ON kyc_verification;
CREATE POLICY kyc_verification_app ON kyc_verification
    FOR ALL TO pgapp_app
    USING (
        EXISTS (
            SELECT 1 FROM tenants t
            WHERE t.id = kyc_verification.tenant_id
              AND t.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM tenants t
            WHERE t.id = kyc_verification.tenant_id
              AND t.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    );

DROP POLICY IF EXISTS kyc_verification_maint ON kyc_verification;
CREATE POLICY kyc_verification_maint ON kyc_verification
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

-- kyc_consent
ALTER TABLE kyc_consent ENABLE ROW LEVEL SECURITY;
ALTER TABLE kyc_consent FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS kyc_consent_app ON kyc_consent;
CREATE POLICY kyc_consent_app ON kyc_consent
    FOR ALL TO pgapp_app
    USING (
        EXISTS (
            SELECT 1 FROM tenants t
            WHERE t.id = kyc_consent.tenant_id
              AND t.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM tenants t
            WHERE t.id = kyc_consent.tenant_id
              AND t.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    );

DROP POLICY IF EXISTS kyc_consent_maint ON kyc_consent;
CREATE POLICY kyc_consent_maint ON kyc_consent
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

-- kyc_audit_log
ALTER TABLE kyc_audit_log ENABLE ROW LEVEL SECURITY;
ALTER TABLE kyc_audit_log FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS kyc_audit_log_app ON kyc_audit_log;
CREATE POLICY kyc_audit_log_app ON kyc_audit_log
    FOR ALL TO pgapp_app
    USING (
        EXISTS (
            SELECT 1 FROM tenants t
            WHERE t.id = kyc_audit_log.tenant_id
              AND t.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM tenants t
            WHERE t.id = kyc_audit_log.tenant_id
              AND t.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    );

DROP POLICY IF EXISTS kyc_audit_log_maint ON kyc_audit_log;
CREATE POLICY kyc_audit_log_maint ON kyc_audit_log
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

-- kyc_webhook_inbox (Background ingestion queue - restricted from broad application pool access)
ALTER TABLE kyc_webhook_inbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE kyc_webhook_inbox FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS kyc_webhook_inbox_app ON kyc_webhook_inbox;
CREATE POLICY kyc_webhook_inbox_app ON kyc_webhook_inbox
    FOR ALL TO pgapp_app
    USING (false)
    WITH CHECK (false);

DROP POLICY IF EXISTS kyc_webhook_inbox_maint ON kyc_webhook_inbox;
CREATE POLICY kyc_webhook_inbox_maint ON kyc_webhook_inbox
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

-- ============================================================================
-- 4. Role Permissions
-- ============================================================================
GRANT SELECT, INSERT, UPDATE, DELETE ON 
    capital_transactions,
    expense_payments,
    manager_advances,
    manager_reimbursements,
    budgets,
    property_finance_settings,
    approval_policies,
    financial_corrections,
    daily_financial_rollups,
    daily_settlement_balances,
    daily_settlement_balance_runs,
    ledger_control_overrides,
    tenant_departures,
    departure_deductions,
    departure_due_adjustments,
    kyc_verification,
    kyc_consent
TO pgapp_app, pgapp_maint, pgapp_worker;

GRANT SELECT, INSERT ON kyc_audit_log TO pgapp_app, pgapp_maint, pgapp_worker;
