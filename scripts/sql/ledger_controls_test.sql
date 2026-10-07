-- Ledger controls regression suite (ADR-015 / ADR-016, migrations 043 + 044).
--
-- Run against a database that has ALL migrations applied:
--   psql "$DATABASE_URL" -v ON_ERROR_STOP=1 -f scripts/sql/ledger_controls_test.sql
-- Exit status is non-zero on the first failed check.
--
-- Safe on any dev/test database: everything happens inside one transaction that
-- ends in ROLLBACK (the journal is append-only, so rollback is the only cleanup).
-- Do not point this at production.
\set ON_ERROR_STOP on
\set QUIET on
BEGIN;

CREATE TEMP TABLE _checks (label text);

-- expect_error: run sql inside a savepoint, force deferred constraints to fire,
-- and require the given SQLSTATE.
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

-- expect_ok: statement must succeed AND pass deferred constraints.
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

CREATE FUNCTION pg_temp.assert_eq(p_label text, p_actual numeric, p_expected numeric) RETURNS void AS $$
BEGIN
  IF p_actual IS DISTINCT FROM p_expected THEN
    RAISE EXCEPTION 'FAIL [%]: expected % got %', p_label, p_expected, p_actual;
  END IF;
  INSERT INTO _checks VALUES (p_label);
END;
$$ LANGUAGE plpgsql;

-- post: one balanced two-line journal entry (single source, single property).
CREATE FUNCTION pg_temp.post(p_prop uuid, p_type text, p_src uuid, p_at timestamptz,
                             p_dr text, p_cr text, p_amt bigint) RETURNS void AS $$
BEGIN
  INSERT INTO financial_journal_entries (property_id, account_code, debit_paise, source_type, source_id, line_kind, occurred_at)
  VALUES (p_prop, p_dr, p_amt, p_type, p_src, 'dr', p_at);
  INSERT INTO financial_journal_entries (property_id, account_code, credit_paise, source_type, source_id, line_kind, occurred_at)
  VALUES (p_prop, p_cr, p_amt, p_type, p_src, 'cr', p_at);
END;
$$ LANGUAGE plpgsql;

-- Fixtures: A = controls, B = statements/reconciliation, C = second property.
INSERT INTO properties (id, name, owner_phone, upi_vpa, owner_name, owner_email, invite_code) VALUES
 ('a0000000-0000-0000-0000-00000000000a','LC-A','9100000001','a@upi','O','a@x.in','LCA'),
 ('b0000000-0000-0000-0000-00000000000b','LC-B','9100000002','b@upi','O','b@x.in','LCB'),
 ('c0000000-0000-0000-0000-00000000000c','LC-C','9100000003','c@upi','O','c@x.in','LCC');

-- ═════════════════════════ C-2  double entry (per property) ═════════════════════════
SELECT pg_temp.expect_ok('C-2 balanced pair commits',
  $q$SELECT pg_temp.post('a0000000-0000-0000-0000-00000000000a','payment',gen_random_uuid(),'2026-09-05+00','bank','rent_revenue',100)$q$);

SELECT pg_temp.expect_error('C-2 single unbalanced line rejected (LG002)',
  $q$INSERT INTO financial_journal_entries (property_id,account_code,debit_paise,source_type,source_id,line_kind,occurred_at)
     VALUES ('a0000000-0000-0000-0000-00000000000a','bank',50,'payment',gen_random_uuid(),'dr','2026-09-05+00')$q$, 'LG002');

-- The 043 flaw: debit in property A, credit in property C, same source, nets to zero.
SELECT pg_temp.expect_error('C-2 cross-property netting rejected (LG002)',
  $q$WITH s AS (SELECT gen_random_uuid() AS id),
     d AS (INSERT INTO financial_journal_entries (property_id,account_code,debit_paise,source_type,source_id,line_kind,occurred_at)
           SELECT 'a0000000-0000-0000-0000-00000000000a','bank',70,'payment',id,'dr','2026-09-06+00' FROM s RETURNING 1)
     INSERT INTO financial_journal_entries (property_id,account_code,credit_paise,source_type,source_id,line_kind,occurred_at)
     SELECT 'c0000000-0000-0000-0000-00000000000c','rent_revenue',70,'payment',id,'cr','2026-09-06+00' FROM s$q$, 'LG002');

-- Multi-line balanced entry (departure-settlement shape: 1 debit, 2 credits).
SELECT pg_temp.expect_ok('C-2 multi-line balanced entry commits',
  $q$WITH s AS (SELECT gen_random_uuid() AS id)
     INSERT INTO financial_journal_entries (property_id,account_code,debit_paise,credit_paise,source_type,source_id,line_kind,occurred_at)
     SELECT 'a0000000-0000-0000-0000-00000000000a', v.acct, v.dr, v.cr, 'departure_settlement', s.id, v.kind, '2026-09-07+00'
       FROM s, (VALUES ('deposit_liability',400,0,'deposit_release'),
                       ('damages_income',0,50,'damages_recovery'),
                       ('refund_payable',0,350,'tenant_refund_payable')) AS v(acct,dr,cr,kind)$q$);

-- ═════════════════════════ C-1  append-only journal ═════════════════════════
SELECT pg_temp.expect_error('C-1 UPDATE blocked (LG003)',
  $q$UPDATE financial_journal_entries SET debit_paise = 1 WHERE property_id='a0000000-0000-0000-0000-00000000000a' AND debit_paise > 0$q$, 'LG003');
SELECT pg_temp.expect_error('C-1 DELETE blocked (LG003)',
  $q$DELETE FROM financial_journal_entries WHERE property_id='a0000000-0000-0000-0000-00000000000a'$q$, 'LG003');

-- Maintenance DELETE of a whole balanced source succeeds AND is audited.
SELECT pg_temp.expect_ok('C-1 DELETE allowed with maintenance GUC',
  $q$SELECT set_config('app.ledger_maintenance','on',true);
     SELECT set_config('app.actor','lc-test',true);
     DELETE FROM financial_journal_entries WHERE source_type='departure_settlement' AND property_id='a0000000-0000-0000-0000-00000000000a';
     SELECT set_config('app.ledger_maintenance','off',true)$q$);
SELECT pg_temp.assert_eq('C-1 maintenance DELETE logged (3 rows, actor lc-test)',
  (SELECT count(*) FROM ledger_control_overrides WHERE event_type='journal_delete' AND actor='lc-test' AND property_id='a0000000-0000-0000-0000-00000000000a'), 3);

SELECT pg_temp.expect_error('Audit log UPDATE blocked (LG003)',
  $q$UPDATE ledger_control_overrides SET detail='x'$q$, 'LG003');
SELECT pg_temp.expect_error('Audit log DELETE blocked even with maintenance GUC (LG003)',
  $q$SELECT set_config('app.ledger_maintenance','on',true); DELETE FROM ledger_control_overrides$q$, 'LG003');

-- ═════════════════════════ C-3 / C-4  closed periods ═════════════════════════
INSERT INTO period_tie_outs (property_id, period_month, status, closed_at)
VALUES ('a0000000-0000-0000-0000-00000000000a', '2026-08', 'closed', now());

-- the ComputeTieOut upsert shape against a closed row (the 043 regression)
SELECT pg_temp.expect_error('C-3 upsert on closed period blocked (LG004)',
  $q$INSERT INTO period_tie_outs (property_id,period_month,recon_total_paise,status)
     VALUES ('a0000000-0000-0000-0000-00000000000a','2026-08',5,'closed')
     ON CONFLICT (property_id,period_month) DO UPDATE SET recon_total_paise=EXCLUDED.recon_total_paise$q$, 'LG004');
SELECT pg_temp.expect_error('C-3 DELETE closed period blocked (LG004)',
  $q$DELETE FROM period_tie_outs WHERE property_id='a0000000-0000-0000-0000-00000000000a' AND period_month='2026-08'$q$, 'LG004');

SELECT pg_temp.expect_error('C-4 posting into closed period rejected (LG001)',
  $q$SELECT pg_temp.post('a0000000-0000-0000-0000-00000000000a','payment',gen_random_uuid(),'2026-08-20+00','bank','rent_revenue',9)$q$, 'LG001');
SELECT pg_temp.expect_error('C-4 last UTC second of closed month rejected (LG001)',
  $q$SELECT pg_temp.post('a0000000-0000-0000-0000-00000000000a','payment',gen_random_uuid(),'2026-08-31 23:59:59+00','bank','rent_revenue',9)$q$, 'LG001');
SELECT pg_temp.expect_ok('C-4 first UTC instant of next month accepted',
  $q$SELECT pg_temp.post('a0000000-0000-0000-0000-00000000000a','payment',gen_random_uuid(),'2026-09-01 00:00:00+00','bank','rent_revenue',9)$q$);
-- 03:00 IST on 1 Sep is 21:30 UTC on 31 Aug: the lock follows UTC (same as finance.PeriodBounds).
SELECT pg_temp.expect_error('C-4 boundary is UTC not IST (LG001)',
  $q$SELECT pg_temp.post('a0000000-0000-0000-0000-00000000000a','payment',gen_random_uuid(),'2026-09-01 03:00:00+05:30','bank','rent_revenue',9)$q$, 'LG001');
SELECT pg_temp.expect_ok('C-4 other property unaffected by A''s closed month',
  $q$SELECT pg_temp.post('c0000000-0000-0000-0000-00000000000c','payment',gen_random_uuid(),'2026-08-20+00','bank','rent_revenue',9)$q$);

SELECT pg_temp.expect_ok('C-4 posting into closed period allowed with reopen GUC',
  $q$SELECT set_config('app.reopen_period','on',true);
     SELECT set_config('app.actor','lc-test',true);
     SELECT pg_temp.post('a0000000-0000-0000-0000-00000000000a','payment',gen_random_uuid(),'2026-08-21+00','bank','rent_revenue',11);
     SELECT set_config('app.reopen_period','off',true)$q$);
SELECT pg_temp.assert_eq('C-4 override posting logged (2 lines)',
  (SELECT count(*) FROM ledger_control_overrides WHERE event_type='posting_into_closed_period' AND actor='lc-test'), 2);

SELECT pg_temp.expect_ok('C-3 reopen with GUC allowed and audited',
  $q$SELECT set_config('app.reopen_period','on',true);
     SELECT set_config('app.actor','lc-test',true);
     UPDATE period_tie_outs SET status='open', closed_at=NULL WHERE property_id='a0000000-0000-0000-0000-00000000000a' AND period_month='2026-08';
     SELECT set_config('app.reopen_period','off',true)$q$);
SELECT pg_temp.assert_eq('C-3 reopen logged as period_reopened',
  (SELECT count(*) FROM ledger_control_overrides WHERE event_type='period_reopened' AND period_month='2026-08' AND actor='lc-test'), 1);
SELECT pg_temp.expect_ok('C-4 posting accepted once period is open again',
  $q$SELECT pg_temp.post('a0000000-0000-0000-0000-00000000000a','payment',gen_random_uuid(),'2026-08-22+00','bank','rent_revenue',13)$q$);

-- ═════════════════════════ Statements: one realistic month (property B) ═════════════════════════
-- Posting shapes copied from the Go Mirror*/service code (see journal-entry-catalogue.md).
-- Sept 2026 activity plus one August receipt to prove period scoping.
DO $$
DECLARE p uuid := 'b0000000-0000-0000-0000-00000000000b';
BEGIN
  PERFORM pg_temp.post(p,'payment',gen_random_uuid(),'2026-08-15+00','bank','rent_revenue',1000);          -- Aug rent
  PERFORM pg_temp.post(p,'capital',gen_random_uuid(),'2026-09-01+00','bank','owner_capital',500000);       -- capital in
  PERFORM pg_temp.post(p,'payment',gen_random_uuid(),'2026-09-02+00','bank','rent_revenue',100000);        -- rent via bank
  PERFORM pg_temp.post(p,'payment',gen_random_uuid(),'2026-09-03+00','gateway_clearing','rent_revenue',80000); -- rent via gateway
  PERFORM pg_temp.post(p,'payment',gen_random_uuid(),'2026-09-04+00','bank','deposit_liability',40000);    -- deposit
  PERFORM pg_temp.post(p,'payment',gen_random_uuid(),'2026-09-05+00','bank','utility_recovery_revenue',5000);
  PERFORM pg_temp.post(p,'expense',gen_random_uuid(),'2026-09-06+00','operating_expense','accounts_payable',30000);
  PERFORM pg_temp.post(p,'expense_payment',gen_random_uuid(),'2026-09-07+00','accounts_payable','bank',30000);
  PERFORM pg_temp.post(p,'refund',gen_random_uuid(),'2026-09-08+00','rent_revenue','gateway_clearing',10000);
  -- gateway settlement: bank 58,500 + fees 1,500 = clearing 60,000 (3 lines)
  INSERT INTO financial_journal_entries (property_id,account_code,debit_paise,credit_paise,source_type,source_id,line_kind,occurred_at)
  SELECT p, v.acct, v.dr, v.cr, 'gateway_settlement', s.id, v.kind, '2026-09-09+00'
    FROM (SELECT gen_random_uuid() AS id) s,
         (VALUES ('bank',58500,0,'settlement_bank_dr'),
                 ('payment_processing_expense',1500,0,'settlement_fee_dr'),
                 ('gateway_clearing',0,60000,'settlement_clearing_cr')) AS v(acct,dr,cr,kind);
  -- departure settlement: deposit 40,000 = damages 5,000 + refund 35,000
  INSERT INTO financial_journal_entries (property_id,account_code,debit_paise,credit_paise,source_type,source_id,line_kind,occurred_at)
  SELECT p, v.acct, v.dr, v.cr, 'departure_settlement', s.id, v.kind, '2026-09-10+00'
    FROM (SELECT gen_random_uuid() AS id) s,
         (VALUES ('deposit_liability',40000,0,'deposit_release'),
                 ('damages_income',0,5000,'damages_recovery'),
                 ('refund_payable',0,35000,'tenant_refund_payable')) AS v(acct,dr,cr,kind);
  PERFORM pg_temp.post(p,'payout_settlement',gen_random_uuid(),'2026-09-11+00','refund_payable','bank',35000);
END $$;
SET CONSTRAINTS ALL IMMEDIATE; SET CONSTRAINTS ALL DEFERRED;

SELECT pg_temp.assert_eq('TB total debits = total credits',
  (SELECT sum(debit_paise) - sum(credit_paise) FROM ledger_trial_balance('b0000000-0000-0000-0000-00000000000b','2026-10-01+00')), 0);
SELECT pg_temp.assert_eq('TB has no unclassified accounts',
  (SELECT count(*) FROM ledger_trial_balance('b0000000-0000-0000-0000-00000000000b','2026-10-01+00') WHERE account_class='unclassified'), 0);
SELECT pg_temp.assert_eq('TB bank balance',
  (SELECT balance_paise FROM ledger_trial_balance('b0000000-0000-0000-0000-00000000000b','2026-10-01+00') WHERE account_code='bank'), 639500);

-- Income statement, September only (August receipt excluded)
SELECT pg_temp.assert_eq('IS Sept rent_revenue (100000+80000-10000)',
  (SELECT amount_paise FROM ledger_income_statement('b0000000-0000-0000-0000-00000000000b','2026-09-01+00','2026-10-01+00') WHERE account_code='rent_revenue'), 170000);
SELECT pg_temp.assert_eq('IS Sept total_revenue',
  (SELECT amount_paise FROM ledger_income_statement('b0000000-0000-0000-0000-00000000000b','2026-09-01+00','2026-10-01+00') WHERE section='total_revenue'), 180000);
SELECT pg_temp.assert_eq('IS Sept total_expense',
  (SELECT amount_paise FROM ledger_income_statement('b0000000-0000-0000-0000-00000000000b','2026-09-01+00','2026-10-01+00') WHERE section='total_expense'), 31500);
SELECT pg_temp.assert_eq('IS Sept net_income',
  (SELECT amount_paise FROM ledger_income_statement('b0000000-0000-0000-0000-00000000000b','2026-09-01+00','2026-10-01+00') WHERE section='net_income'), 148500);
SELECT pg_temp.assert_eq('IS Aug net_income isolated',
  (SELECT amount_paise FROM ledger_income_statement('b0000000-0000-0000-0000-00000000000b','2026-08-01+00','2026-09-01+00') WHERE section='net_income'), 1000);

-- Balance sheet at end of Sept (excl. Aug? no: cumulative, so includes Aug 1,000)
SELECT pg_temp.assert_eq('BS accounting equation holds (check_difference = 0)',
  (SELECT amount_paise FROM ledger_balance_sheet('b0000000-0000-0000-0000-00000000000b','2026-10-01+00') WHERE section='check_difference'), 0);
SELECT pg_temp.assert_eq('BS total_assets (bank 639500 + clearing 10000)',
  (SELECT amount_paise FROM ledger_balance_sheet('b0000000-0000-0000-0000-00000000000b','2026-10-01+00') WHERE section='total_assets'), 649500);
SELECT pg_temp.assert_eq('BS total_equity (capital 500000 + earnings 149500)',
  (SELECT amount_paise FROM ledger_balance_sheet('b0000000-0000-0000-0000-00000000000b','2026-10-01+00') WHERE section='total_equity'), 649500);
SELECT pg_temp.assert_eq('BS as at 31 Aug: only the August receipt exists',
  (SELECT amount_paise FROM ledger_balance_sheet('b0000000-0000-0000-0000-00000000000b','2026-09-01+00') WHERE section='total_assets'), 1000);
SELECT pg_temp.assert_eq('BS as at 31 Aug balances',
  (SELECT amount_paise FROM ledger_balance_sheet('b0000000-0000-0000-0000-00000000000b','2026-09-01+00') WHERE section='check_difference'), 0);

-- Cash flow (cash + bank), September
SELECT pg_temp.assert_eq('CF opening cash = August receipt',
  (SELECT amount_paise FROM ledger_cash_flow('b0000000-0000-0000-0000-00000000000b','2026-09-01+00','2026-10-01+00') WHERE section='opening_cash'), 1000);
SELECT pg_temp.assert_eq('CF financing (capital)',
  (SELECT amount_paise FROM ledger_cash_flow('b0000000-0000-0000-0000-00000000000b','2026-09-01+00','2026-10-01+00') WHERE section='financing'), 500000);
SELECT pg_temp.assert_eq('CF net change (operating 138500 + financing 500000)',
  (SELECT amount_paise FROM ledger_cash_flow('b0000000-0000-0000-0000-00000000000b','2026-09-01+00','2026-10-01+00') WHERE section='net_change'), 638500);
SELECT pg_temp.assert_eq('CF closing cash = balance-sheet bank',
  (SELECT amount_paise FROM ledger_cash_flow('b0000000-0000-0000-0000-00000000000b','2026-09-01+00','2026-10-01+00') WHERE section='closing_cash'),
  (SELECT balance_paise FROM ledger_trial_balance('b0000000-0000-0000-0000-00000000000b','2026-10-01+00') WHERE account_code='bank'));
SELECT pg_temp.assert_eq('CF independent closing check = 0',
  (SELECT amount_paise FROM ledger_cash_flow('b0000000-0000-0000-0000-00000000000b','2026-09-01+00','2026-10-01+00') WHERE section='check_difference'), 0);

-- ═════════════════════════ Reconciliation aging (as of 2026-10-03) ═════════════════════════
INSERT INTO bank_transactions (property_id, txn_id, amount_paise, row_type, txn_date, dedup_hash, status) VALUES
 ('b0000000-0000-0000-0000-00000000000b','UTR-OLD', 7000000,'credit','2026-08-24','lc-h1','unmatched'),        -- 40d, >= INR 50,000
 ('b0000000-0000-0000-0000-00000000000b','UTR-NEW', 1200,   'credit','2026-10-01','lc-h2','suggested_match'),  -- 2d
 ('b0000000-0000-0000-0000-00000000000b','UTR-DONE',9999999,'credit','2026-07-01','lc-h3','matched'),          -- must be ignored
 ('b0000000-0000-0000-0000-00000000000b','UTR-DEB', 9999999,'debit', '2026-07-01','lc-h4','unmatched');        -- must be ignored
INSERT INTO gateway_settlements (property_id, cf_settlement_id, gross_amount_paise, net_amount_paise, settlement_status, settled_on, reconciliation_status)
VALUES ('b0000000-0000-0000-0000-00000000000b','CFS-1',300000,298000,'SUCCESS','2026-09-20+00','discrepancy'),
       ('b0000000-0000-0000-0000-00000000000b','CFS-2',100000, 99000,'SUCCESS','2026-09-20+00','matched');     -- ignored
INSERT INTO period_tie_outs (property_id, period_month, difference_paise, status)
VALUES ('b0000000-0000-0000-0000-00000000000b','2026-07',250000,'open');                                      -- ends 31 Jul: 64d
INSERT INTO ledger_outbox_events (event_type, property_id, source_id, idempotency_key, failed_at, created_at)
VALUES ('departure_settlement_mirror','b0000000-0000-0000-0000-00000000000b',gen_random_uuid(),'lc-dead',now(),'2026-09-30+00'),
       ('departure_settlement_mirror','b0000000-0000-0000-0000-00000000000b',gen_random_uuid(),'lc-done',NULL,'2026-09-30+00');
UPDATE ledger_outbox_events SET processed_at = now() WHERE idempotency_key = 'lc-done';


-- ═════════════════════════ Completeness fixtures (property B) ═════════════════════════
INSERT INTO tenants (id, property_id, name, rent_amount, status)
VALUES ('d0000000-0000-0000-0000-000000000001','b0000000-0000-0000-0000-00000000000b','LC Tenant',0,'pending_allocation');
INSERT INTO dues (id, due_code, tenant_id, property_id, amount, original_amount, period_start, period_end, due_date)
VALUES ('d1000000-0000-0000-0000-000000000001','LC-DUE-1','d0000000-0000-0000-0000-000000000001','b0000000-0000-0000-0000-00000000000b',1000,1000,'2026-09-01','2026-09-30','2026-09-05');
INSERT INTO payments (id, property_id, due_id, tenant_id, amount, matched_by, is_unapplied, provider, cf_payment_id, provider_payment_id, created_at) VALUES
 ('e0000000-0000-0000-0000-000000000001','b0000000-0000-0000-0000-00000000000b','d1000000-0000-0000-0000-000000000001','d0000000-0000-0000-0000-000000000001',1000,'cashfree',false,'cashfree','CF-1','CF-1', now() - interval '2 days'), -- posted
 ('e0000000-0000-0000-0000-000000000002','b0000000-0000-0000-0000-00000000000b','d1000000-0000-0000-0000-000000000001','d0000000-0000-0000-0000-000000000001',2000,'cashfree',false,'cashfree','CF-2','CF-2', now() - interval '2 days'), -- UNPOSTED gateway
 ('e0000000-0000-0000-0000-000000000003','b0000000-0000-0000-0000-00000000000b','d1000000-0000-0000-0000-000000000001','d0000000-0000-0000-0000-000000000001',3000,'cash',    false,'manual',  NULL,   NULL,   now() - interval '2 days'), -- unposted cash (reported, not escalated)
 ('e0000000-0000-0000-0000-000000000004','b0000000-0000-0000-0000-00000000000b','d1000000-0000-0000-0000-000000000001','d0000000-0000-0000-0000-000000000001',4000,'cashfree',false,'cashfree','CF-4','CF-4', now()),                     -- in flight (inside grace)
 ('e0000000-0000-0000-0000-000000000005','b0000000-0000-0000-0000-00000000000b',NULL,'d0000000-0000-0000-0000-000000000001',5000,'cashfree',true, 'cashfree','CF-5','CF-5', now() - interval '2 days'); -- unapplied, posted
SELECT pg_temp.post('b0000000-0000-0000-0000-00000000000b','payment',           'e0000000-0000-0000-0000-000000000001','2026-09-14+00','gateway_clearing','rent_revenue',1000);
SELECT pg_temp.post('b0000000-0000-0000-0000-00000000000b','unapplied_payment', 'e0000000-0000-0000-0000-000000000005','2026-09-14+00','gateway_clearing','unapplied_receipts',5000);
-- orphan posting: journal exists, payment row does not (payment tx rolled back after the ledger committed)
INSERT INTO financial_journal_entries (property_id,account_code,debit_paise,source_type,source_id,line_kind,occurred_at,created_at) VALUES
 ('b0000000-0000-0000-0000-00000000000b','gateway_clearing',700,'payment','e0000000-0000-0000-0000-000000000099','dr','2026-09-12+00', now() - interval '2 days');
INSERT INTO financial_journal_entries (property_id,account_code,credit_paise,source_type,source_id,line_kind,occurred_at,created_at) VALUES
 ('b0000000-0000-0000-0000-00000000000b','rent_revenue',700,'payment','e0000000-0000-0000-0000-000000000099','cr','2026-09-12+00', now() - interval '2 days');
-- orphan inside the grace window: legitimately in flight, must NOT be flagged
SELECT pg_temp.post('b0000000-0000-0000-0000-00000000000b','payment','e0000000-0000-0000-0000-000000000098','2026-09-15+00','gateway_clearing','rent_revenue',50);
-- refunds: R1 succeeded for 10,000 but only 6,000 journaled; R2 fully journaled; R3 not succeeded
INSERT INTO gateway_refunds (id, payment_id, property_id, amount_paise, status, reason, source, provider, updated_at) VALUES
 ('f0000000-0000-0000-0000-000000000001','e0000000-0000-0000-0000-000000000001','b0000000-0000-0000-0000-00000000000b',10000,'succeeded','x','system','cashfree', now() - interval '2 days'),
 ('f0000000-0000-0000-0000-000000000002','e0000000-0000-0000-0000-000000000001','b0000000-0000-0000-0000-00000000000b',3000,'succeeded','x','system','cashfree', now() - interval '2 days'),
 ('f0000000-0000-0000-0000-000000000003','e0000000-0000-0000-0000-000000000001','b0000000-0000-0000-0000-00000000000b',500,'initiated','x','system','cashfree', now() - interval '2 days');
SELECT pg_temp.post('b0000000-0000-0000-0000-00000000000b','refund','f0000000-0000-0000-0000-000000000001','2026-09-13+00','rent_revenue','gateway_clearing',6000);
SELECT pg_temp.post('b0000000-0000-0000-0000-00000000000b','refund','f0000000-0000-0000-0000-000000000002','2026-09-13+00','rent_revenue','gateway_clearing',3000);

-- Why a multi-due refund loses its 2nd allocation: MirrorRefund reuses (source_type, source_id, line_kind)
-- for every allocation, uq_journal_source_line rejects the 2nd insert, and Go maps that to "already posted".
SELECT pg_temp.expect_error('REFUND 2nd allocation of same refund id hits uq_journal_source_line (23505)',
  $q$SELECT pg_temp.post('b0000000-0000-0000-0000-00000000000b','refund','f0000000-0000-0000-0000-000000000001','2026-09-13+00','rent_revenue','gateway_clearing',4000)$q$, '23505');

SELECT pg_temp.assert_eq('COMPLETE unposted payments: exactly the two old unposted (gateway + cash)',
  (SELECT count(*) FROM ledger_unposted_payments('b0000000-0000-0000-0000-00000000000b')), 2);
SELECT pg_temp.assert_eq('COMPLETE in-flight payment inside grace is not flagged',
  (SELECT count(*) FROM ledger_unposted_payments('b0000000-0000-0000-0000-00000000000b') WHERE payment_id='e0000000-0000-0000-0000-000000000004'), 0);
SELECT pg_temp.assert_eq('COMPLETE unapplied payment with unapplied_payment journal is not flagged',
  (SELECT count(*) FROM ledger_unposted_payments('b0000000-0000-0000-0000-00000000000b') WHERE payment_id='e0000000-0000-0000-0000-000000000005'), 0);
SELECT pg_temp.assert_eq('COMPLETE orphan posting found; in-flight one ignored',
  (SELECT count(*) FROM ledger_orphan_postings('b0000000-0000-0000-0000-00000000000b')), 1);
SELECT pg_temp.assert_eq('COMPLETE orphan amount = 700',
  (SELECT amount_paise FROM ledger_orphan_postings('b0000000-0000-0000-0000-00000000000b')), 700);
SELECT pg_temp.assert_eq('COMPLETE refund gap: only R1, gap = 4000',
  (SELECT sum(gap_paise) FROM ledger_refund_posting_gaps('b0000000-0000-0000-0000-00000000000b')), 4000);
SELECT pg_temp.assert_eq('COMPLETE refund gap count = 1 (R2 whole, R3 not succeeded)',
  (SELECT count(*) FROM ledger_refund_posting_gaps('b0000000-0000-0000-0000-00000000000b')), 1);

SELECT pg_temp.assert_eq('RECON only open items returned (8 = 5 aging + unposted + orphan + refund gap)',
  (SELECT count(*) FROM ledger_reconciling_items('b0000000-0000-0000-0000-00000000000b','2026-10-03')), 8);
SELECT pg_temp.assert_eq('RECON old bank credit: 31-60 bucket, investigate, owner (amount)',
  (SELECT count(*) FROM ledger_reconciling_items('b0000000-0000-0000-0000-00000000000b','2026-10-03')
    WHERE ref='UTR-OLD' AND age_days=40 AND age_bucket='31-60 aging' AND category='investigate' AND escalation='owner'), 1);
SELECT pg_temp.assert_eq('RECON fresh bank credit: current, timing, no escalation',
  (SELECT count(*) FROM ledger_reconciling_items('b0000000-0000-0000-0000-00000000000b','2026-10-03')
    WHERE ref='UTR-NEW' AND age_days=2 AND age_bucket='0-30 current' AND category='timing' AND escalation='none'), 1);
SELECT pg_temp.assert_eq('RECON open tie-out difference 64d: 61-90 overdue, manager',
  (SELECT count(*) FROM ledger_reconciling_items('b0000000-0000-0000-0000-00000000000b','2026-10-03')
    WHERE item_type='period_tie_out_difference' AND age_days=64 AND age_bucket='61-90 overdue' AND escalation='manager'), 1);
SELECT pg_temp.assert_eq('RECON dead-lettered ledger event escalates to owner',
  (SELECT count(*) FROM ledger_reconciling_items('b0000000-0000-0000-0000-00000000000b','2026-10-03')
    WHERE item_type='ledger_event_dead_letter' AND escalation='owner' AND category='investigate'), 1);
SELECT pg_temp.assert_eq('RECON gateway discrepancy investigated, no escalation',
  (SELECT count(*) FROM ledger_reconciling_items('b0000000-0000-0000-0000-00000000000b','2026-10-03')
    WHERE item_type='gateway_settlement_unreconciled' AND ref='CFS-1' AND category='investigate' AND escalation='none'), 1);

SELECT pg_temp.assert_eq('RECON unposted GATEWAY payment escalates to owner',
  (SELECT count(*) FROM ledger_reconciling_items('b0000000-0000-0000-0000-00000000000b','2026-10-03')
    WHERE item_type='payment_unposted' AND ref='e0000000-0000-0000-0000-000000000002' AND amount_paise=2000 AND escalation='owner' AND category='investigate'), 1);
SELECT pg_temp.assert_eq('RECON unposted CASH payment reported by function but not auto-escalated',
  (SELECT count(*) FROM ledger_reconciling_items('b0000000-0000-0000-0000-00000000000b','2026-10-03') WHERE ref='e0000000-0000-0000-0000-000000000003'), 0);
SELECT pg_temp.assert_eq('RECON orphan posting escalates to owner',
  (SELECT count(*) FROM ledger_reconciling_items('b0000000-0000-0000-0000-00000000000b','2026-10-03')
    WHERE item_type='payment_posting_orphan' AND amount_paise=700 AND escalation='owner'), 1);
SELECT pg_temp.assert_eq('RECON refund posting gap escalates to owner with gap amount',
  (SELECT count(*) FROM ledger_reconciling_items('b0000000-0000-0000-0000-00000000000b','2026-10-03')
    WHERE item_type='refund_posting_gap' AND amount_paise=4000 AND escalation='owner'), 1);
SELECT pg_temp.assert_eq('RECON property filter isolates (property C has none)',
  (SELECT count(*) FROM ledger_reconciling_items('c0000000-0000-0000-0000-00000000000c','2026-10-03')), 0);

-- ═════════════════════════ Repair paths documented in the close runbook ═════════════════════════
-- (1) Orphan posting: reverse it with source_type 'correction'; the detective control must clear.
INSERT INTO financial_journal_entries (property_id, account_code, debit_paise, credit_paise, source_type, source_id, line_kind, occurred_at)
SELECT property_id, account_code, credit_paise, debit_paise, 'correction', source_id, 'reversal_' || line_kind, now()
  FROM financial_journal_entries
 WHERE source_type = 'payment' AND source_id = 'e0000000-0000-0000-0000-000000000099';
SET CONSTRAINTS ALL IMMEDIATE; SET CONSTRAINTS ALL DEFERRED;
SELECT pg_temp.assert_eq('REPAIR orphan cleared by correction entry',
  (SELECT count(*) FROM ledger_orphan_postings('b0000000-0000-0000-0000-00000000000b')), 0);
SELECT pg_temp.assert_eq('REPAIR orphan reversal nets the original to zero (Dr rent_revenue 700 offsets Cr)',
  (SELECT COALESCE(SUM(credit_paise) - SUM(debit_paise), 0) FROM financial_journal_entries WHERE source_id = 'e0000000-0000-0000-0000-000000000099'), 0);
-- (2) Refund gap: post the missing allocation under the same refund id with distinct line kinds.
INSERT INTO financial_journal_entries (property_id, account_code, debit_paise, source_type, source_id, line_kind, occurred_at)
VALUES ('b0000000-0000-0000-0000-00000000000b', 'rent_revenue', 4000, 'refund', 'f0000000-0000-0000-0000-000000000001', 'refund_reversal_dr_alloc2', now());
INSERT INTO financial_journal_entries (property_id, account_code, credit_paise, source_type, source_id, line_kind, occurred_at)
VALUES ('b0000000-0000-0000-0000-00000000000b', 'gateway_clearing', 4000, 'refund', 'f0000000-0000-0000-0000-000000000001', 'gateway_clearing_cr_alloc2', now());
SET CONSTRAINTS ALL IMMEDIATE; SET CONSTRAINTS ALL DEFERRED;
SELECT pg_temp.assert_eq('REPAIR refund gap cleared by posting the missing allocation',
  (SELECT count(*) FROM ledger_refund_posting_gaps('b0000000-0000-0000-0000-00000000000b')), 0);
-- (3) Unposted gateway payment: posting the entry itself clears it.
SELECT pg_temp.post('b0000000-0000-0000-0000-00000000000b','payment','e0000000-0000-0000-0000-000000000002', now(),'gateway_clearing','rent_revenue',2000);
SELECT pg_temp.assert_eq('REPAIR unposted gateway payment cleared by posting it',
  (SELECT count(*) FROM ledger_unposted_payments('b0000000-0000-0000-0000-00000000000b') WHERE matched_by='cashfree'), 0);
SELECT pg_temp.assert_eq('REPAIR all money-integrity items resolved; only aging items remain (5)',
  (SELECT count(*) FROM ledger_reconciling_items('b0000000-0000-0000-0000-00000000000b','2026-10-03')), 5);
SELECT pg_temp.assert_eq('REPAIR balance sheet still balances after repairs',
  (SELECT amount_paise FROM ledger_balance_sheet('b0000000-0000-0000-0000-00000000000b','2099-01-01+00') WHERE section='check_difference'), 0);

SELECT 'ALL ' || count(*) || ' LEDGER CONTROL CHECKS PASSED' AS result FROM _checks;
ROLLBACK;
