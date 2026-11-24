SET lock_timeout = '5s';

-- Migration 127: Complete Multi-Tenant Row Level Security & Financial Immutability Hardening
-- Standard: ASD-STE100
-- Purpose:
--   1. Enforce strict table-specific privileges: restore and enforce revocation of UPDATE/DELETE
--      on all append-only financial and ledger records (capital_transactions, expense_payments,
--      payout_records, payout_record_items, daily_settlement_balance_runs, departure_due_adjustments,
--      financial_journal_entries).
--   2. Add database-level immutability triggers on daily_settlement_balance_runs and departure_due_adjustments.
--   3. Create structured, deduplicated payout_reconciliation_incidents table.
--   4. Enforce fail-closed Row Level Security (RLS) across all property-scoped, parent-linked,
--      and KYC tables (including kyc_in_flight_lock, kyc_failure_log, and kyc_otp_attempt_log).

-- ============================================================================
-- 1. Database-Level Immutability Triggers
-- ============================================================================
CREATE OR REPLACE FUNCTION trg_immutable_audit_table()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION '% records are immutable and cannot be updated or deleted', TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

DO $$ BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'trg_balance_runs_immutable') THEN
        CREATE TRIGGER trg_balance_runs_immutable
        BEFORE UPDATE OR DELETE ON daily_settlement_balance_runs
        FOR EACH ROW EXECUTE FUNCTION trg_immutable_audit_table();
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'trg_departure_due_adj_immutable') THEN
        CREATE TRIGGER trg_departure_due_adj_immutable
        BEFORE UPDATE OR DELETE ON departure_due_adjustments
        FOR EACH ROW EXECUTE FUNCTION trg_immutable_audit_table();
    END IF;
END $$;

-- ============================================================================
-- 2. Structured Payout Reconciliation Incidents Table
-- ============================================================================
CREATE TABLE IF NOT EXISTS payout_reconciliation_incidents (
    id BIGSERIAL PRIMARY KEY,
    fingerprint VARCHAR(64) NOT NULL UNIQUE,
    incident_type VARCHAR(64) NOT NULL,
    batch_id UUID REFERENCES payout_batches(id) ON DELETE CASCADE,
    payout_item_id UUID REFERENCES payout_items(id) ON DELETE CASCADE,
    expected_transfer_id TEXT,
    received_transfer_id TEXT,
    details TEXT NOT NULL,
    evidence_payload_hash VARCHAR(64),
    occurrence_count INT NOT NULL DEFAULT 1,
    status VARCHAR(24) NOT NULL DEFAULT 'open',
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_payout_recon_incidents_batch ON payout_reconciliation_incidents(batch_id, status);
CREATE INDEX IF NOT EXISTS idx_payout_recon_incidents_item ON payout_reconciliation_incidents(payout_item_id, status);

ALTER TABLE payout_reconciliation_incidents ENABLE ROW LEVEL SECURITY;
ALTER TABLE payout_reconciliation_incidents FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS payout_reconciliation_incidents_app ON payout_reconciliation_incidents;
CREATE POLICY payout_reconciliation_incidents_app ON payout_reconciliation_incidents
    FOR ALL TO pgapp_app
    USING (
        EXISTS (
            SELECT 1 FROM payout_batches pb
            WHERE pb.id = payout_reconciliation_incidents.batch_id
              AND pb.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM payout_batches pb
            WHERE pb.id = payout_reconciliation_incidents.batch_id
              AND pb.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    );

DROP POLICY IF EXISTS payout_reconciliation_incidents_maint ON payout_reconciliation_incidents;
CREATE POLICY payout_reconciliation_incidents_maint ON payout_reconciliation_incidents
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

-- ============================================================================
-- 3. Direct Property-Scoped Tables
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
        'tenant_departures',
        'approval_requests',
        'staff_profiles',
        'leave_policies',
        'attendance_records',
        'wage_calculations',
        'kpi_snapshots',
        'roi_snapshots',
        'leakage_events',
        'recommendations',
        'forecast_snapshots',
        'meal_prep_actuals',
        'property_settings',
        'ledger_outbox_events',
        'period_tie_outs',
        'reward_liability_transactions',
        'expense_import_suggestions',
        'payment_commands'
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
-- 4. Hybrid Scoped Table: financial_categories (Global Defaults + Property Custom)
-- ============================================================================
ALTER TABLE financial_categories ENABLE ROW LEVEL SECURITY;
ALTER TABLE financial_categories FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS financial_categories_app ON financial_categories;
CREATE POLICY financial_categories_app ON financial_categories
    FOR ALL TO pgapp_app
    USING (property_id IS NULL OR property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
    WITH CHECK (property_id IS NOT NULL AND property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid);

DROP POLICY IF EXISTS financial_categories_maint ON financial_categories;
CREATE POLICY financial_categories_maint ON financial_categories
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

-- ============================================================================
-- 5. Parent-Linked Tables (departure_deductions, departure_due_adjustments)
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
-- 6. KYC Tables Isolation (Core & Auxiliary)
-- ============================================================================

-- Helper macro for all tenant-parented KYC tables
DO $$
DECLARE
    tbl TEXT;
    kyc_tables TEXT[] := ARRAY[
        'kyc_verification',
        'kyc_consent',
        'kyc_audit_log',
        'kyc_in_flight_lock',
        'kyc_failure_log',
        'kyc_otp_attempt_log'
    ];
BEGIN
    FOREACH tbl IN ARRAY kyc_tables LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY;', tbl);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY;', tbl);

        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', tbl || '_app', tbl);
        EXECUTE format('
            CREATE POLICY %I ON %I
            FOR ALL TO pgapp_app
            USING (
                EXISTS (
                    SELECT 1 FROM tenants t
                    WHERE t.id = %I.tenant_id
                      AND t.property_id = NULLIF(current_setting(''app.current_property_id'', true), '''')::uuid
                )
            )
            WITH CHECK (
                EXISTS (
                    SELECT 1 FROM tenants t
                    WHERE t.id = %I.tenant_id
                      AND t.property_id = NULLIF(current_setting(''app.current_property_id'', true), '''')::uuid
                )
            );
        ', tbl || '_app', tbl, tbl, tbl);

        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', tbl || '_maint', tbl);
        EXECUTE format('
            CREATE POLICY %I ON %I
            FOR ALL TO pgapp_maint, pgapp_worker
            USING (true)
            WITH CHECK (true);
        ', tbl || '_maint', tbl);
    END LOOP;
END $$;

-- kyc_webhook_inbox (Background ingestion queue - isolated completely from pgapp_app)
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
-- 7. Role Privileges: Table-Specific Permissions & Immutability Enforcement
-- ============================================================================

-- Revoke mutation rights on append-only financial and audit tables from all runtime roles
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_app') THEN
        REVOKE UPDATE, DELETE ON 
            capital_transactions,
            expense_payments,
            payout_records,
            payout_record_items,
            daily_settlement_balance_runs,
            departure_due_adjustments,
            financial_journal_entries,
            kyc_audit_log
        FROM pgapp_app;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_maint') THEN
        REVOKE UPDATE, DELETE ON 
            capital_transactions,
            expense_payments,
            payout_records,
            payout_record_items,
            daily_settlement_balance_runs,
            departure_due_adjustments,
            financial_journal_entries,
            kyc_audit_log
        FROM pgapp_maint;
    END IF;

    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_worker') THEN
        REVOKE UPDATE, DELETE ON 
            capital_transactions,
            expense_payments,
            payout_records,
            payout_record_items,
            daily_settlement_balance_runs,
            departure_due_adjustments,
            financial_journal_entries,
            kyc_audit_log
        FROM pgapp_worker;
    END IF;
END $$;

-- Grant SELECT, INSERT on append-only tables
GRANT SELECT, INSERT ON 
    capital_transactions,
    expense_payments,
    payout_records,
    payout_record_items,
    daily_settlement_balance_runs,
    departure_due_adjustments,
    financial_journal_entries,
    kyc_audit_log
TO pgapp_app, pgapp_maint, pgapp_worker;

-- Grant read/write on mutable tables
GRANT SELECT, INSERT, UPDATE, DELETE ON 
    manager_advances,
    manager_reimbursements,
    budgets,
    property_finance_settings,
    approval_policies,
    financial_corrections,
    daily_financial_rollups,
    daily_settlement_balances,
    ledger_control_overrides,
    tenant_departures,
    departure_deductions,
    approval_requests,
    staff_profiles,
    leave_policies,
    attendance_records,
    wage_calculations,
    kpi_snapshots,
    roi_snapshots,
    leakage_events,
    recommendations,
    forecast_snapshots,
    meal_prep_actuals,
    property_settings,
    ledger_outbox_events,
    period_tie_outs,
    reward_liability_transactions,
    expense_import_suggestions,
    payment_commands,
    financial_categories,
    kyc_verification,
    kyc_consent,
    kyc_in_flight_lock,
    kyc_failure_log,
    kyc_otp_attempt_log,
    payout_reconciliation_incidents
TO pgapp_app, pgapp_maint, pgapp_worker;

GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO pgapp_app, pgapp_maint, pgapp_worker;
