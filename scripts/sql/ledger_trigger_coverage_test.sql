-- Trigger coverage suite (Track R.8 follow-up; mutation finding P1-2).
--
-- Each block targets one trigger that the original ledger controls suite did
-- not exercise. Every check fails when its trigger is disabled, so the suite
-- kills the mutant.
--
-- Run against a database that has ALL migrations applied:
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f scripts/sql/ledger_trigger_coverage_test.sql
--
-- Safe on dev and test databases only. Everything runs in one transaction that
-- ends in ROLLBACK. Do not point this at production.

\set ON_ERROR_STOP on
\set QUIET on
BEGIN;

CREATE TEMP TABLE _checks (label text);

-- expect_error: statement must fail with the given SQLSTATE.
CREATE FUNCTION pg_temp.expect_error(p_label text, p_sql text, p_state text) RETURNS void AS $$
BEGIN
  BEGIN
    EXECUTE p_sql;
    EXECUTE 'SET CONSTRAINTS ALL IMMEDIATE';
    EXECUTE 'SET CONSTRAINTS ALL DEFERRED';
  EXCEPTION WHEN OTHERS THEN
    IF SQLSTATE = p_state THEN
      INSERT INTO _checks VALUES (p_label);
      RETURN;
    END IF;
    RAISE EXCEPTION 'FAIL [%]: expected SQLSTATE % but got % (%)', p_label, p_state, SQLSTATE, SQLERRM;
  END;
  RAISE EXCEPTION 'FAIL [%]: expected SQLSTATE % but statement succeeded', p_label, p_state;
END;
$$ LANGUAGE plpgsql;

-- expect_ok: statement must succeed and pass deferred constraints.
CREATE FUNCTION pg_temp.expect_ok(p_label text, p_sql text) RETURNS void AS $$
BEGIN
  EXECUTE p_sql;
  EXECUTE 'SET CONSTRAINTS ALL IMMEDIATE';
  EXECUTE 'SET CONSTRAINTS ALL DEFERRED';
  INSERT INTO _checks VALUES (p_label);
EXCEPTION WHEN OTHERS THEN
  RAISE EXCEPTION 'FAIL [%]: unexpected error % (%)', p_label, SQLSTATE, SQLERRM;
END;
$$ LANGUAGE plpgsql;

-- expect_rows: a count query must return the expected value.
CREATE FUNCTION pg_temp.expect_rows(p_label text, p_sql text, p_expected bigint) RETURNS void AS $$
DECLARE v bigint;
BEGIN
  EXECUTE p_sql INTO v;
  IF v IS DISTINCT FROM p_expected THEN
    RAISE EXCEPTION 'FAIL [%]: expected % got %', p_label, p_expected, v;
  END IF;
  INSERT INTO _checks VALUES (p_label);
END;
$$ LANGUAGE plpgsql;

-- Fixtures. Property D is new to this suite. Period 2026-06 stays open.
-- Period 2026-07 is closed AFTER all July fixtures are inserted.
INSERT INTO properties (id, name, owner_phone, upi_vpa, owner_name, owner_email, invite_code)
VALUES ('d0000000-0000-0000-0000-00000000000d','TC-D','9100000004','d@upi','O','d@x.in','TCD');

INSERT INTO users (id, phone, role, property_id)
VALUES ('d1000000-0000-0000-0000-000000000001','9100000005','owner','d0000000-0000-0000-0000-00000000000d');

INSERT INTO tenants (id, property_id, name, phone, rent_amount, due_day, status)
VALUES ('d2000000-0000-0000-0000-000000000001','d0000000-0000-0000-0000-00000000000d',
        'TC Tenant','9100000006', 1500000, 5, 'active');

INSERT INTO dues (id, due_code, tenant_id, property_id, kind, amount, original_amount,
                  period_start, period_end, due_date, status)
VALUES ('d3000000-0000-0000-0000-000000000001','TC-DUE-1','d2000000-0000-0000-0000-000000000001',
        'd0000000-0000-0000-0000-00000000000d','rent',1500000,1500000,
        '2026-06-01','2026-06-30','2026-06-05','open');

-- June payment with a due: auto-allocation trigger must create one allocation row.
INSERT INTO payments (id, due_id, tenant_id, amount, matched_by, matched_at, created_at,
                      is_unapplied, property_id, provider)
VALUES ('d4000000-0000-0000-0000-000000000001','d3000000-0000-0000-0000-000000000001',
        'd2000000-0000-0000-0000-000000000001', 500000, 'manual',
        '2026-06-10+00', '2026-06-10+00', false, 'd0000000-0000-0000-0000-00000000000d', 'manual');

-- Pre-closure payment used as parent for July-era corrections and refunds.
INSERT INTO payments (id, tenant_id, amount, matched_by, matched_at, created_at, is_unapplied, property_id, provider)
VALUES ('d4000000-0000-0000-0000-000000000002','d2000000-0000-0000-0000-000000000001',
        100000, 'manual', '2026-06-11+00', '2026-06-11+00', true,
        'd0000000-0000-0000-0000-00000000000d', 'manual');

-- Payment intent with one intent-due row. Sync and direct-update triggers act on it.
INSERT INTO payment_intents (id, due_id, provider, provider_order_id, amount_paise, status)
VALUES ('d5000000-0000-0000-0000-000000000001','d3000000-0000-0000-0000-000000000001',
        'cashfree','TC-ORD-1',500000,'created');
INSERT INTO payment_intent_dues (id, payment_intent_id, due_id, amount_paise, status)
VALUES ('d6000000-0000-0000-0000-000000000001','d5000000-0000-0000-0000-000000000001',
        'd3000000-0000-0000-0000-000000000001',500000,'created');

-- kyc audit row (tenant FK). Immutability triggers act on it.
INSERT INTO kyc_audit_log (tenant_id, actor, action, detail_hash)
VALUES ('d2000000-0000-0000-0000-000000000001','test','TC_EVENT','h1');

-- Settlement balance run. Immutability trigger acts on it.
INSERT INTO daily_settlement_balance_runs (property_id, recon_date, is_balanced)
VALUES ('d0000000-0000-0000-0000-00000000000d','2026-06-30', true);

-- Financial correction in June. Immutability trigger acts on it.
INSERT INTO financial_corrections (id, property_id, original_payment_id, reason, corrected_by, created_at)
VALUES ('d7000000-0000-0000-0000-000000000001','d0000000-0000-0000-0000-00000000000d',
        'd4000000-0000-0000-0000-000000000002','TC reason','d1000000-0000-0000-0000-000000000001',
        '2026-06-12+00');

-- July fixtures. Inserted while July is still open.
INSERT INTO gateway_refunds (id, payment_id, property_id, amount_paise, status, reason, created_at, source)
VALUES ('d8000000-0000-0000-0000-000000000001','d4000000-0000-0000-0000-000000000002',
        'd0000000-0000-0000-0000-00000000000d', 10000, 'pending', 'TC refund', '2026-07-03+00', 'system');

-- Close July for property D.
INSERT INTO period_tie_outs (property_id, period_month, status, closed_at)
VALUES ('d0000000-0000-0000-0000-00000000000d', '2026-07', 'closed', now());

-- ═════════ T1  trg_payments_auto_allocate (AFTER INSERT on payments) ═════════
SELECT pg_temp.expect_rows('T1 auto-allocate creates one allocation row for due payment',
  $q$SELECT count(*) FROM payment_allocations
      WHERE payment_id='d4000000-0000-0000-0000-000000000001'
        AND due_id='d3000000-0000-0000-0000-000000000001' AND amount_paise=500000$q$, 1);

-- ═════════ T2  trg_payments_period_lock (BEFORE INSERT on payments, closed July) ═════════
SELECT pg_temp.expect_error('T2 payment into closed period rejected (LG001)',
  $q$INSERT INTO payments (tenant_id, amount, matched_by, matched_at, created_at, is_unapplied, property_id, provider)
     VALUES ('d2000000-0000-0000-0000-000000000001', 1000, 'manual', '2026-07-15+00', '2026-07-15+00', true,
             'd0000000-0000-0000-0000-00000000000d', 'manual')$q$, 'LG001');

-- ═════════ T3  trg_sync_payment_intent_dues_status (AFTER UPDATE on payment_intents) ═════════
SELECT pg_temp.expect_ok('T3 intent status change propagates to intent-dues',
  $q$UPDATE payment_intents SET status='paid' WHERE id='d5000000-0000-0000-0000-000000000001'$q$);
SELECT pg_temp.expect_rows('T3 propagated status is PAID',
  $q$SELECT count(*) FROM payment_intent_dues
      WHERE id='d6000000-0000-0000-0000-000000000001' AND status='paid'$q$, 1);

-- ═════════ T4  trg_prevent_direct_payment_intent_dues_status_update (BEFORE UPDATE) ═════════
SELECT pg_temp.expect_error('T4 direct status update on intent-due rejected',
  $q$UPDATE payment_intent_dues SET status='FORGED' WHERE id='d6000000-0000-0000-0000-000000000001'$q$, 'P0001');

-- ═════════ T5  trg_kyc_audit_immutable (BEFORE UPDATE OR DELETE on kyc_audit_log) ═════════
SELECT pg_temp.expect_error('T5 kyc audit UPDATE blocked',
  $q$UPDATE kyc_audit_log SET action='TAMPERED' WHERE tenant_id='d2000000-0000-0000-0000-000000000001'$q$, 'P0001');
SELECT pg_temp.expect_error('T5 kyc audit DELETE blocked',
  $q$DELETE FROM kyc_audit_log WHERE tenant_id='d2000000-0000-0000-0000-000000000001'$q$, 'P0001');

-- ═════════ T6  trg_gateway_refunds_period_lock (BEFORE INSERT on gateway_refunds) ═════════
SELECT pg_temp.expect_error('T6 refund into closed period rejected (LG001)',
  $q$INSERT INTO gateway_refunds (payment_id, property_id, amount_paise, status, reason, created_at, source)
     VALUES ('d4000000-0000-0000-0000-000000000002','d0000000-0000-0000-0000-00000000000d',
             5000,'pending','TC late refund','2026-07-20+00','system')$q$, 'LG001');

-- ═════════ T7  trg_refund_allocations_period_lock (BEFORE INSERT on refund_allocations) ═════════
SELECT pg_temp.expect_error('T7 refund allocation into closed period rejected (LG001)',
  $q$INSERT INTO refund_allocations (refund_id, due_id, amount_paise)
     VALUES ('d8000000-0000-0000-0000-000000000001','d3000000-0000-0000-0000-000000000001',1000)$q$, 'LG001');

-- ═════════ T8  trg_prevent_modification (BEFORE UPDATE OR DELETE on daily_settlement_balance_runs) ═════════
SELECT pg_temp.expect_error('T8 settlement balance run UPDATE blocked',
  $q$UPDATE daily_settlement_balance_runs SET is_balanced=false
      WHERE property_id='d0000000-0000-0000-0000-00000000000d'$q$, 'P0001');
SELECT pg_temp.expect_error('T8 settlement balance run DELETE blocked',
  $q$DELETE FROM daily_settlement_balance_runs
      WHERE property_id='d0000000-0000-0000-0000-00000000000d'$q$, 'P0001');

-- ═════════ T9  trg_financial_corrections_immutable (BEFORE UPDATE OR DELETE) ═════════
SELECT pg_temp.expect_error('T9 financial correction UPDATE blocked (LG003)',
  $q$UPDATE financial_corrections SET reason='rewritten' WHERE id='d7000000-0000-0000-0000-000000000001'$q$, 'LG003');
SELECT pg_temp.expect_error('T9 financial correction DELETE blocked (LG003)',
  $q$DELETE FROM financial_corrections WHERE id='d7000000-0000-0000-0000-000000000001'$q$, 'LG003');

-- ═════════ T10  trg_financial_corrections_period_lock (BEFORE INSERT on financial_corrections) ═════════
SELECT pg_temp.expect_error('T10 correction into closed period rejected (LG001)',
  $q$INSERT INTO financial_corrections (property_id, original_payment_id, reason, corrected_by, created_at)
     VALUES ('d0000000-0000-0000-0000-00000000000d','d4000000-0000-0000-0000-000000000002',
             'late','d1000000-0000-0000-0000-000000000001','2026-07-21+00')$q$, 'LG001');

-- ═════════ Summary ═════════
SELECT CASE
  WHEN (SELECT count(*) FROM _checks) = 14
  THEN 'ALL 14 TRIGGER COVERAGE CHECKS PASSED'
  ELSE 'UNEXPECTED CHECK COUNT: ' || (SELECT count(*) FROM _checks)
END AS result;

ROLLBACK;
