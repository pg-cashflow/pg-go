-- RLS property-scope RATCHET (complements rls_coverage_test.sql).
--
-- rls_coverage_test.sql asserts only the 13 core tables. This ratchet asserts that
-- EVERY table carrying a property_id column is either:
--   (a) FORCE ROW LEVEL SECURITY enabled with at least one policy, or
--   (b) listed in the baseline below (known unprotected; app-layer scoping only).
--
-- Rules:
--   * A NEW property-scoped table without RLS fails the build.
--   * A baseline table that becomes protected emits a NOTICE: delete it from the
--     baseline in the same change, so the baseline only ever shrinks.
--   * Adding to the baseline is a deliberate, reviewed act; it needs a justification
--     in docs/compliance/ledger-controls-matrix.md or an ADR.
--
-- Run: psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f scripts/sql/rls_property_scope_ratchet.sql
-- Safe on dev/test: wrapped in a transaction that ends in ROLLBACK.

\set ON_ERROR_STOP on
BEGIN;

CREATE TEMP TABLE _rls_baseline (tbl text PRIMARY KEY) ON COMMIT DROP;
INSERT INTO _rls_baseline (tbl) VALUES
  ('approval_policies'),
  ('approval_requests'),
  ('attendance_records'),
  ('budgets'),
  ('capital_transactions'),
  ('daily_financial_rollups'),
  ('daily_settlement_balance_runs'),
  ('daily_settlement_balances'),
  ('expense_import_suggestions'),
  ('expense_payments'),
  ('financial_categories'),
  ('financial_corrections'),
  ('floors'),
  ('forecast_snapshots'),
  ('hazards'),
  ('import_logs'),
  ('inspections'),
  ('join_requests'),
  ('kpi_snapshots'),
  ('leakage_events'),
  ('leave_policies'),
  ('ledger_control_overrides'),
  ('ledger_outbox_events'),
  ('manager_advances'),
  ('manager_reimbursements'),
  ('meal_prep_actuals'),
  ('meal_rsvps'),
  ('menu_polls'),
  ('meter_readings'),
  ('notifications'),
  ('outbox_events'),
  ('period_tie_outs'),
  ('point_rules'),
  ('points_ledger'),
  ('property_finance_settings'),
  ('property_gamification_settings'),
  ('property_settings'),
  ('recommendations'),
  ('redemptions'),
  ('referrals'),
  ('reward_liability_transactions'),
  ('rewards_catalog'),
  ('roi_snapshots'),
  ('rooms'),
  ('search_documents'),
  ('staff_profiles'),
  ('tenant_departures'),
  ('tenant_streaks'),
  ('unmatched_gateway_receipts'),
  ('users'),
  ('vendor_inspections'),
  ('violations'),
  ('wage_calculations');

DO $$
DECLARE
    r record;
    regressions text[] := ARRAY[]::text[];
    stale       text[] := ARRAY[]::text[];
    protected   boolean;
    in_baseline boolean;
    scoped_total int := 0;
BEGIN
    FOR r IN
        SELECT c.relname
        FROM pg_class c
        WHERE c.relnamespace = 'public'::regnamespace
          AND c.relkind = 'r'
          AND EXISTS (
              SELECT 1 FROM information_schema.columns ic
              WHERE ic.table_schema = 'public'
                AND ic.table_name = c.relname
                AND ic.column_name = 'property_id')
    LOOP
        scoped_total := scoped_total + 1;
        SELECT c.relrowsecurity AND c.relforcerowsecurity
               AND EXISTS (SELECT 1 FROM pg_policy p WHERE p.polrelid = c.oid)
          INTO protected
          FROM pg_class c WHERE c.relname = r.relname AND c.relnamespace = 'public'::regnamespace;
        in_baseline := EXISTS (SELECT 1 FROM _rls_baseline b WHERE b.tbl = r.relname);

        IF NOT protected AND NOT in_baseline THEN
            regressions := regressions || r.relname;
        ELSIF protected AND in_baseline THEN
            stale := stale || r.relname;
        END IF;
    END LOOP;

    IF scoped_total = 0 THEN
        RAISE EXCEPTION 'FAIL: no property-scoped tables found; schema not migrated?';
    END IF;
    IF array_length(regressions, 1) IS NOT NULL THEN
        RAISE EXCEPTION 'FAIL: property-scoped tables without FORCE RLS + policy and not in baseline: %', regressions;
    END IF;
    IF array_length(stale, 1) IS NOT NULL THEN
        RAISE NOTICE 'baseline entries now protected; remove them from the baseline: %', stale;
    END IF;
    RAISE NOTICE 'property-scope ratchet OK: % scoped tables, % baseline entries', scoped_total, (SELECT count(*) FROM _rls_baseline);
END
$$;

ROLLBACK;
