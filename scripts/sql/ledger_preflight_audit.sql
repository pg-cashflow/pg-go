-- Ledger pre-flight audit (read-only). Run BEFORE relying on migrations 043/044
-- against a database that already holds journal data, and monthly thereafter.
--
--   psql "$DATABASE_URL" -f scripts/sql/ledger_preflight_audit.sql
--
-- The ledger triggers only guard NEW writes; they never rewrite history. This
-- script tells you whether history already violates the rules they enforce.
-- Exit status is non-zero if any check finds violations (so CI/cron can alert).
-- Nothing here modifies data.
\set ON_ERROR_STOP on
\pset footer off

CREATE TEMP TABLE _audit (check_name text, violations bigint, detail text);

-- 1. Every (property, source_type, source_id) must balance.
INSERT INTO _audit
SELECT 'unbalanced_source', count(*),
       'sources where SUM(debit) <> SUM(credit); post a reversing/correcting entry per runbook'
  FROM (SELECT 1 FROM financial_journal_entries
         GROUP BY property_id, source_type, source_id
        HAVING SUM(debit_paise) <> SUM(credit_paise)) x;

-- 2. A source must not span properties (043 netted these to zero and passed them).
INSERT INTO _audit
SELECT 'cross_property_source', count(*),
       'one source_type/source_id posted into more than one property'
  FROM (SELECT 1 FROM financial_journal_entries
         GROUP BY source_type, source_id
        HAVING count(DISTINCT property_id) > 1) x;

-- 3. Closed periods must have a zero tie-out difference (CloseTieOut enforces this).
INSERT INTO _audit
SELECT 'closed_period_nonzero_difference', count(*),
       'period_tie_outs closed with difference_paise <> 0'
  FROM period_tie_outs WHERE status = 'closed' AND difference_paise <> 0;

-- 4. Postings inserted after their period was closed (evidence the close was not enforced).
INSERT INTO _audit
SELECT 'posted_after_period_close', count(*),
       'journal lines created after the tie-out for their month was closed'
  FROM financial_journal_entries j
  JOIN period_tie_outs t
    ON t.property_id = j.property_id
   AND t.period_month = to_char(j.occurred_at AT TIME ZONE 'UTC', 'YYYY-MM')
   AND t.status = 'closed' AND t.closed_at IS NOT NULL
 WHERE j.created_at > t.closed_at;

-- 5. Accounts missing from ledger_account_class() would silently drop out of statements.
INSERT INTO _audit
SELECT 'unclassified_account', count(DISTINCT account_code),
       coalesce(string_agg(DISTINCT account_code, ', '), '') FROM financial_journal_entries
 WHERE ledger_account_class(account_code) = 'unclassified';

-- 6. Completeness (see ADR-016): gateway payments with no journal, orphan postings, refund gaps.
INSERT INTO _audit
SELECT 'gateway_payment_unposted', count(*), 'matched_by=cashfree payments with no journal (revenue understated)'
  FROM ledger_unposted_payments() WHERE matched_by = 'cashfree';
INSERT INTO _audit
SELECT 'orphan_posting', count(*), 'journal sources whose payment/refund row does not exist (revenue overstated)'
  FROM ledger_orphan_postings();
INSERT INTO _audit
SELECT 'refund_posting_gap', count(*), 'succeeded refunds whose journaled amount differs from the refund amount'
  FROM ledger_refund_posting_gaps();

-- Informational only (a product decision, not a violation): non-gateway payments never posted.
INSERT INTO _audit
SELECT 'info_non_gateway_payment_unposted', 0,
       count(*) || ' cash/manual/other payments have no journal posting; see ADR-016 section 6'
  FROM ledger_unposted_payments() WHERE matched_by <> 'cashfree';

SELECT check_name, violations, detail FROM _audit ORDER BY check_name;

DO $$
DECLARE v bigint;
BEGIN
  SELECT coalesce(sum(violations), 0) INTO v FROM _audit;
  IF v > 0 THEN
    RAISE EXCEPTION 'ledger pre-flight audit: % violation(s) found (see table above)', v;
  END IF;
END $$;
