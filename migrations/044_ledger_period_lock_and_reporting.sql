-- Migration 044: ledger period lock, per-property balance, override audit,
--                financial statements and reconciliation aging (ADR-016)
--
-- Builds on 043 (ADR-015). Forward-only: 043 is NOT edited; the trigger
-- functions it created are replaced here via CREATE OR REPLACE so that every
-- control raises a stable, machine-readable SQLSTATE the application can map.
--
--   SQLSTATE  Control
--   LG001     C-4  posting into a closed period
--   LG002     C-2  unbalanced journal (per source, per property)
--   LG003     C-1  journal is append-only / audit log is immutable
--   LG004     C-3  closed period tie-out is frozen
--
-- Override (maintenance / audited reopen) GUCs, transaction-scoped only:
--   SET LOCAL app.reopen_period     = 'on';   -- modify/post into a closed period
--   SET LOCAL app.ledger_maintenance = 'on';  -- DELETE journal / closed tie-out
--   SET LOCAL app.actor             = '<who>';-- recorded in ledger_control_overrides
-- Every use of an override while it matters is written to ledger_control_overrides.

-- ─────────────────────────────────────────────────────────────────────────────
-- A. Override audit log (immutable, no escape hatch, survives property archive)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE IF NOT EXISTS ledger_control_overrides (
  id           BIGSERIAL PRIMARY KEY,
  event_type   TEXT        NOT NULL,
  property_id  UUID,                      -- deliberately no FK: audit must outlive the property
  period_month CHAR(7),
  detail       TEXT        NOT NULL DEFAULT '',
  actor        TEXT        NOT NULL,
  occurred_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_ledger_control_overrides_prop
  ON ledger_control_overrides (property_id, occurred_at);

CREATE OR REPLACE FUNCTION trg_ledger_override_log_immutable() RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION 'ledger_control_overrides is an immutable audit log (% blocked)', TG_OP
    USING ERRCODE = 'LG003';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_ledger_override_log_immutable ON ledger_control_overrides;
CREATE TRIGGER trg_ledger_override_log_immutable
  BEFORE UPDATE OR DELETE ON ledger_control_overrides
  FOR EACH ROW EXECUTE FUNCTION trg_ledger_override_log_immutable();

CREATE OR REPLACE FUNCTION ledger_log_override(
  p_event text, p_property uuid, p_period text, p_detail text
) RETURNS void AS $$
BEGIN
  INSERT INTO ledger_control_overrides (event_type, property_id, period_month, detail, actor)
  VALUES (
    p_event, p_property, p_period, p_detail,
    COALESCE(NULLIF(current_setting('app.actor', true), ''), session_user::text)
  );
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- B. Re-issue C-1 / C-2 / C-3 with stable SQLSTATEs (+ C-2 per property, C-3 audit)
-- ─────────────────────────────────────────────────────────────────────────────

-- C-1: journal append-only.
CREATE OR REPLACE FUNCTION trg_journal_append_only() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    RAISE EXCEPTION
      'financial_journal_entries is append-only (UPDATE blocked); post a reversing entry instead'
      USING ERRCODE = 'LG003';
  END IF;
  IF COALESCE(current_setting('app.ledger_maintenance', true), 'off') <> 'on' THEN
    RAISE EXCEPTION 'financial_journal_entries is append-only (DELETE blocked)'
      USING ERRCODE = 'LG003';
  END IF;
  PERFORM ledger_log_override('journal_delete', OLD.property_id,
    to_char(OLD.occurred_at AT TIME ZONE 'UTC', 'YYYY-MM'),
    format('deleted %s/%s line_kind=%s dr=%s cr=%s',
           OLD.source_type, OLD.source_id, OLD.line_kind, OLD.debit_paise, OLD.credit_paise));
  RETURN OLD;
END;
$$ LANGUAGE plpgsql;

-- C-2: double entry, now evaluated per (source_type, source_id, property_id).
-- 043 summed across properties, so a source whose lines sat in two different
-- properties could net to zero and pass. Every Mirror* helper posts one source
-- into exactly one property, so the stricter rule has no legitimate false positive.
CREATE OR REPLACE FUNCTION trg_journal_balanced() RETURNS trigger AS $$
DECLARE
  v_debit  bigint;
  v_credit bigint;
BEGIN
  SELECT COALESCE(SUM(debit_paise), 0), COALESCE(SUM(credit_paise), 0)
    INTO v_debit, v_credit
    FROM financial_journal_entries
   WHERE source_type = NEW.source_type
     AND source_id   = NEW.source_id
     AND property_id = NEW.property_id;

  IF v_debit <> v_credit THEN
    RAISE EXCEPTION 'unbalanced journal for %/% (property %): debit=% credit=%',
      NEW.source_type, NEW.source_id, NEW.property_id, v_debit, v_credit
      USING ERRCODE = 'LG002';
  END IF;
  RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- C-3: closed tie-out frozen; any override is audited.
CREATE OR REPLACE FUNCTION trg_tieout_closed_frozen() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF OLD.status = 'closed' THEN
      IF COALESCE(current_setting('app.ledger_maintenance', true), 'off') <> 'on' THEN
        RAISE EXCEPTION 'closed period % cannot be deleted', OLD.period_month
          USING ERRCODE = 'LG004';
      END IF;
      PERFORM ledger_log_override('closed_period_delete', OLD.property_id, OLD.period_month,
                                  'closed tie-out row deleted');
    END IF;
    RETURN OLD;
  END IF;

  IF OLD.status = 'closed' THEN
    IF COALESCE(current_setting('app.reopen_period', true), 'off') <> 'on' THEN
      RAISE EXCEPTION 'period % is closed; reopen requires SET LOCAL app.reopen_period = ''on''',
        OLD.period_month USING ERRCODE = 'LG004';
    END IF;
    PERFORM ledger_log_override(
      CASE WHEN NEW.status <> 'closed' THEN 'period_reopened' ELSE 'closed_period_modified' END,
      OLD.property_id, OLD.period_month,
      format('status %s -> %s, difference_paise %s -> %s',
             OLD.status, NEW.status, OLD.difference_paise, NEW.difference_paise));
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- C. C-4: no postings into a closed period
--    Month key is UTC, matching finance.PeriodBounds (Go) exactly.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION trg_journal_period_lock() RETURNS trigger AS $$
DECLARE
  v_period text := to_char(NEW.occurred_at AT TIME ZONE 'UTC', 'YYYY-MM');
BEGIN
  IF EXISTS (
    SELECT 1 FROM period_tie_outs t
     WHERE t.property_id  = NEW.property_id
       AND t.period_month = v_period
       AND t.status       = 'closed'
  ) THEN
    IF COALESCE(current_setting('app.reopen_period', true), 'off') <> 'on' THEN
      RAISE EXCEPTION 'period % is closed for property %; cannot post %/% (%)',
        v_period, NEW.property_id, NEW.source_type, NEW.source_id, NEW.line_kind
        USING ERRCODE = 'LG001';
    END IF;
    PERFORM ledger_log_override('posting_into_closed_period', NEW.property_id, v_period,
      format('posted %s/%s line_kind=%s dr=%s cr=%s',
             NEW.source_type, NEW.source_id, NEW.line_kind, NEW.debit_paise, NEW.credit_paise));
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_journal_period_lock ON financial_journal_entries;
CREATE TRIGGER trg_journal_period_lock
  BEFORE INSERT ON financial_journal_entries
  FOR EACH ROW EXECUTE FUNCTION trg_journal_period_lock();

-- ─────────────────────────────────────────────────────────────────────────────
-- D. Financial statements (derived from the journal; read-only, STABLE)
--    All "to" bounds are EXCLUSIVE, same [from, to) convention as PeriodBounds.
--    Amounts are integer paise. No closing entries exist in this ledger, so
--    equity carries cumulative earnings as a derived line instead of posted
--    retained earnings.
-- ─────────────────────────────────────────────────────────────────────────────

-- Chart-of-accounts classification (mirrors internal/domain/finance.go Acct*).
-- Anything unknown is 'unclassified' so it surfaces in tests instead of vanishing.
CREATE OR REPLACE FUNCTION ledger_account_class(p_code text) RETURNS text AS $$
  SELECT CASE p_code
    WHEN 'cash'                       THEN 'asset'
    WHEN 'bank'                       THEN 'asset'
    WHEN 'tenant_receivable'          THEN 'asset'
    WHEN 'gateway_clearing'           THEN 'asset'
    WHEN 'deposit_liability'          THEN 'liability'
    WHEN 'accounts_payable'           THEN 'liability'
    WHEN 'manager_advance_payable'    THEN 'liability'
    WHEN 'reward_liability'           THEN 'liability'
    WHEN 'unapplied_receipts'         THEN 'liability'
    WHEN 'refund_payable'             THEN 'liability'
    WHEN 'owner_capital'              THEN 'equity'
    WHEN 'rent_revenue'               THEN 'revenue'
    WHEN 'utility_recovery_revenue'   THEN 'revenue'
    WHEN 'damages_income'             THEN 'revenue'
    WHEN 'interest_income'            THEN 'revenue'
    WHEN 'non_pg_other_income'        THEN 'revenue'
    WHEN 'operating_expense'          THEN 'expense'
    WHEN 'loyalty_expense'            THEN 'expense'
    WHEN 'payment_processing_expense' THEN 'expense'
    WHEN 'gateway_adjustment'         THEN 'other'   -- net debit = loss, net credit = gain
    ELSE 'unclassified'
  END;
$$ LANGUAGE sql IMMUTABLE;

-- Trial balance. balance_paise is debit-positive (debits - credits).
CREATE OR REPLACE FUNCTION ledger_trial_balance(p_property uuid, p_to timestamptz DEFAULT now())
RETURNS TABLE (account_code text, account_class text,
               debit_paise bigint, credit_paise bigint, balance_paise bigint) AS $$
  SELECT j.account_code::text,
         ledger_account_class(j.account_code),
         COALESCE(SUM(j.debit_paise), 0)::bigint,
         COALESCE(SUM(j.credit_paise), 0)::bigint,
         (COALESCE(SUM(j.debit_paise), 0) - COALESCE(SUM(j.credit_paise), 0))::bigint
    FROM financial_journal_entries j
   WHERE j.property_id = p_property
     AND j.occurred_at < p_to
   GROUP BY j.account_code
   ORDER BY j.account_code;
$$ LANGUAGE sql STABLE;

-- Income statement for [p_from, p_to). Rows: revenue lines, other (gateway
-- adjustments, net), expense lines, then total_revenue / total_expense /
-- net_income summary rows (account_code NULL).
CREATE OR REPLACE FUNCTION ledger_income_statement(p_property uuid, p_from timestamptz, p_to timestamptz)
RETURNS TABLE (section text, account_code text, amount_paise bigint, sort_order int) AS $$
  WITH lines AS (
    SELECT j.account_code::text AS account_code,
           ledger_account_class(j.account_code) AS cls,
           COALESCE(SUM(j.debit_paise), 0)  AS dr,
           COALESCE(SUM(j.credit_paise), 0) AS cr
      FROM financial_journal_entries j
     WHERE j.property_id = p_property
       AND j.occurred_at >= p_from AND j.occurred_at < p_to
     GROUP BY j.account_code
  ), pl AS (
    SELECT cls AS section, account_code,
           CASE cls WHEN 'expense' THEN dr - cr ELSE cr - dr END AS amt,
           CASE cls WHEN 'revenue' THEN 10 WHEN 'other' THEN 20 ELSE 30 END AS ord
      FROM lines WHERE cls IN ('revenue', 'other', 'expense')
  ), totals AS (
    SELECT COALESCE(SUM(amt) FILTER (WHERE section = 'revenue'), 0) AS rev,
           COALESCE(SUM(amt) FILTER (WHERE section = 'other'),   0) AS oth,
           COALESCE(SUM(amt) FILTER (WHERE section = 'expense'), 0) AS exp
      FROM pl
  )
  SELECT section, account_code, amt::bigint, ord FROM pl
  UNION ALL SELECT 'total_revenue', NULL, rev::bigint,            40 FROM totals
  UNION ALL SELECT 'total_expense', NULL, exp::bigint,            50 FROM totals
  UNION ALL SELECT 'net_income',    NULL, (rev + oth - exp)::bigint, 60 FROM totals
  ORDER BY 4, 2;
$$ LANGUAGE sql STABLE;

-- Balance sheet as at (exclusive) p_to. Equity includes derived current_earnings
-- (cumulative net income since inception). check_difference must be 0.
CREATE OR REPLACE FUNCTION ledger_balance_sheet(p_property uuid, p_to timestamptz DEFAULT now())
RETURNS TABLE (section text, account_code text, amount_paise bigint, sort_order int) AS $$
  WITH tb AS (
    SELECT account_code, account_class, balance_paise FROM ledger_trial_balance(p_property, p_to)
  ), bs AS (
    SELECT account_class AS section,
           account_code,
           CASE account_class WHEN 'asset' THEN balance_paise
                              WHEN 'unclassified' THEN balance_paise
                              ELSE -balance_paise END AS amt,
           CASE account_class WHEN 'asset' THEN 10 WHEN 'liability' THEN 20
                              WHEN 'equity' THEN 30 ELSE 35 END AS ord
      FROM tb WHERE account_class IN ('asset', 'liability', 'equity', 'unclassified')
  ), earnings AS (
    SELECT COALESCE(-SUM(balance_paise), 0) AS amt
      FROM tb WHERE account_class IN ('revenue', 'expense', 'other')
  ), sums AS (
    SELECT COALESCE(SUM(amt) FILTER (WHERE section = 'asset'), 0)        AS assets,
           COALESCE(SUM(amt) FILTER (WHERE section = 'liability'), 0)    AS liabs,
           COALESCE(SUM(amt) FILTER (WHERE section = 'equity'), 0)       AS equity,
           COALESCE(SUM(amt) FILTER (WHERE section = 'unclassified'), 0) AS unclassified
      FROM bs
  )
  SELECT section, account_code, amt::bigint, ord FROM bs
  UNION ALL SELECT 'equity', 'current_earnings', amt::bigint, 31 FROM earnings
  UNION ALL SELECT 'total_assets', NULL, assets::bigint, 40 FROM sums
  UNION ALL SELECT 'total_liabilities', NULL, liabs::bigint, 41 FROM sums
  UNION ALL SELECT 'total_equity', NULL, (equity + (SELECT amt FROM earnings))::bigint, 42 FROM sums
  UNION ALL SELECT 'check_difference', NULL,
         (assets + unclassified - liabs - equity - (SELECT amt FROM earnings))::bigint, 50 FROM sums
  ORDER BY 4, 2;
$$ LANGUAGE sql STABLE;

-- Cash flow, direct method, for cash-equivalents = {cash, bank}.
-- gateway_clearing (funds in transit at the processor) is deliberately NOT a
-- cash equivalent here; it is a policy choice recorded in ADR-016.
-- financing = source_type 'capital'; everything else is operating. The chart of
-- accounts has no fixed assets, so there is no investing section.
CREATE OR REPLACE FUNCTION ledger_cash_flow(p_property uuid, p_from timestamptz, p_to timestamptz)
RETURNS TABLE (section text, label text, amount_paise bigint, sort_order int) AS $$
  WITH cash AS (
    SELECT source_type::text AS source_type,
           (debit_paise - credit_paise) AS delta, occurred_at
      FROM financial_journal_entries
     WHERE property_id = p_property AND account_code IN ('cash', 'bank')
  ), opening AS (
    SELECT COALESCE(SUM(delta), 0) AS amt FROM cash WHERE occurred_at < p_from
  ), period AS (
    SELECT CASE WHEN source_type = 'capital' THEN 'financing' ELSE 'operating' END AS section,
           source_type AS label, SUM(delta) AS amt
      FROM cash WHERE occurred_at >= p_from AND occurred_at < p_to
     GROUP BY source_type
  ), closing_independent AS (
    SELECT COALESCE(SUM(delta), 0) AS amt FROM cash WHERE occurred_at < p_to
  )
  SELECT 'opening_cash', NULL::text, amt::bigint, 0 FROM opening
  UNION ALL SELECT section, label, amt::bigint, CASE section WHEN 'operating' THEN 10 ELSE 20 END FROM period
  UNION ALL SELECT 'net_change', NULL, COALESCE(SUM(amt), 0)::bigint, 30 FROM period
  UNION ALL SELECT 'closing_cash', NULL, ((SELECT amt FROM opening) + COALESCE((SELECT SUM(amt) FROM period), 0))::bigint, 40
  UNION ALL SELECT 'check_difference', NULL,
         ((SELECT amt FROM opening) + COALESCE((SELECT SUM(amt) FROM period), 0)
          - (SELECT amt FROM closing_independent))::bigint, 50
  ORDER BY 4, 2;
$$ LANGUAGE sql STABLE;

-- ─────────────────────────────────────────────────────────────────────────────
-- D2. Completeness / detective controls (ADR-016)
--     The payment write and the ledger write are two separate commits: the
--     finance store is pool-bound, so MirrorPayment/MirrorRefund commit on their
--     own connection, BEFORE the webhook transaction commits. Either side can
--     therefore fail alone. Preventive DB controls (C-1..C-4) cannot see a row
--     that was never inserted, so these detective functions measure the gap.
--     p_grace skips rows young enough to be legitimately in flight.
--       unposted : payment committed, no journal           (revenue understated)
--       orphan   : journal committed, no payment/refund    (revenue overstated);
--                  cleared by a 'correction' reversal (see close runbook)
--       refund gap: succeeded refund, journaled amount <> refund amount
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION ledger_unposted_payments(
  p_property uuid DEFAULT NULL,
  p_from timestamptz DEFAULT '-infinity',
  p_to   timestamptz DEFAULT 'infinity',
  p_grace interval DEFAULT '15 minutes'
) RETURNS TABLE (payment_id uuid, property_id uuid, matched_by text, amount_paise bigint,
                 matched_at timestamptz, is_unapplied boolean, expected_source_type text) AS $$
  SELECT p.id,
         COALESCE(d.property_id, t.property_id),
         p.matched_by::text,
         p.amount::bigint,
         p.matched_at,
         p.is_unapplied,
         CASE WHEN p.is_unapplied THEN 'unapplied_payment' ELSE 'payment' END
    FROM payments p
    JOIN tenants t   ON t.id = p.tenant_id
    LEFT JOIN dues d ON d.id = p.due_id
   WHERE p.amount > 0
     AND p.matched_at >= p_from AND p.matched_at < p_to
     AND p.created_at < now() - p_grace
     AND (p_property IS NULL OR COALESCE(d.property_id, t.property_id) = p_property)
     AND NOT EXISTS (
           SELECT 1 FROM financial_journal_entries j
            WHERE j.source_id = p.id
              AND j.source_type = CASE WHEN p.is_unapplied THEN 'unapplied_payment' ELSE 'payment' END)
   ORDER BY p.matched_at;
$$ LANGUAGE sql STABLE;

CREATE OR REPLACE FUNCTION ledger_orphan_postings(
  p_property uuid DEFAULT NULL,
  p_grace interval DEFAULT '15 minutes'
) RETURNS TABLE (property_id uuid, source_type text, source_id uuid,
                 amount_paise bigint, first_posted timestamptz) AS $$
  SELECT j.property_id, j.source_type::text, j.source_id,
         SUM(j.debit_paise)::bigint, MIN(j.created_at)
    FROM financial_journal_entries j
   WHERE j.created_at < now() - p_grace
     AND (p_property IS NULL OR j.property_id = p_property)
     -- resolved once a reversing entry (source_type 'correction', same source_id) exists
     AND NOT EXISTS (SELECT 1 FROM financial_journal_entries c
                      WHERE c.source_type = 'correction' AND c.source_id = j.source_id
                        AND c.property_id = j.property_id)
     AND (   (j.source_type IN ('payment', 'unapplied_payment')
              AND NOT EXISTS (SELECT 1 FROM payments p WHERE p.id = j.source_id))
          OR (j.source_type = 'refund'
              AND NOT EXISTS (SELECT 1 FROM gateway_refunds r WHERE r.id = j.source_id)))
   GROUP BY j.property_id, j.source_type, j.source_id
   ORDER BY MIN(j.created_at);
$$ LANGUAGE sql STABLE;

CREATE OR REPLACE FUNCTION ledger_refund_posting_gaps(
  p_property uuid DEFAULT NULL,
  p_grace interval DEFAULT '15 minutes'
) RETURNS TABLE (refund_id uuid, property_id uuid, refund_amount_paise bigint,
                 posted_paise bigint, gap_paise bigint, updated_at timestamptz) AS $$
  SELECT r.id, r.property_id, r.amount_paise,
         COALESCE(x.posted, 0)::bigint,
         (r.amount_paise - COALESCE(x.posted, 0))::bigint,
         r.updated_at
    FROM gateway_refunds r
    LEFT JOIN LATERAL (
           SELECT SUM(j.credit_paise) AS posted
             FROM financial_journal_entries j
            WHERE j.source_type = 'refund' AND j.source_id = r.id
              AND j.account_code = 'gateway_clearing') x ON true
   WHERE r.status = 'succeeded'
     AND r.updated_at < now() - p_grace
     AND (p_property IS NULL OR r.property_id = p_property)
     AND r.amount_paise <> COALESCE(x.posted, 0)
   ORDER BY r.updated_at;
$$ LANGUAGE sql STABLE;

-- ─────────────────────────────────────────────────────────────────────────────
-- E. Reconciling-items aging (control evidence for the monthly close)
--    All dates are UTC (same convention as the period lock).
--    Buckets: 0-30 current | 31-60 aging | 61-90 overdue | 90+ stale.
--    category: timing (<=5 days, expected to self-clear) | investigate.
--    escalation: none | manager | owner. Defaults (adjust per risk appetite):
--      >= INR 10,000 or age > 60d -> manager ; >= INR 50,000 or age > 90d or
--      any money-integrity item (dead-lettered event, unposted/orphan posting,
--      refund posting gap) -> owner.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION ledger_reconciling_items(p_property uuid DEFAULT NULL, p_as_of date DEFAULT (now() AT TIME ZONE 'UTC')::date)
RETURNS TABLE (item_type text, property_id uuid, ref text, amount_paise bigint,
               originated_on date, age_days int, age_bucket text, category text, escalation text) AS $$
  WITH raw AS (
    SELECT 'bank_credit_unapplied'::text AS item_type, b.property_id, b.txn_id::text AS ref,
           b.amount_paise::bigint AS amount_paise, b.txn_date AS originated_on,
           false AS force_owner, false AS is_discrepancy
      FROM bank_transactions b
     WHERE b.row_type = 'credit' AND b.status IN ('unmatched', 'suggested_match')
       AND NOT b.is_internal_transfer AND NOT b.is_reversal
    UNION ALL
    SELECT 'gateway_settlement_unreconciled', g.property_id, g.cf_settlement_id,
           g.gross_amount_paise::bigint, (COALESCE(g.settled_on, g.created_at) AT TIME ZONE 'UTC')::date,
           false, (g.reconciliation_status = 'discrepancy')
      FROM gateway_settlements g
     WHERE g.reconciliation_status IN ('unmatched', 'discrepancy')
    UNION ALL
    SELECT 'period_tie_out_difference', t.property_id, t.period_month::text,
           abs(t.difference_paise)::bigint,
           ((to_date(t.period_month::text || '-01', 'YYYY-MM-DD') + interval '1 month')::date - 1),
           false, true
      FROM period_tie_outs t
     WHERE t.status = 'open' AND t.difference_paise <> 0
    UNION ALL
    SELECT CASE WHEN o.failed_at IS NULL THEN 'ledger_event_pending' ELSE 'ledger_event_dead_letter' END,
           o.property_id, o.event_type || ':' || o.source_id::text, NULL::bigint,
           (o.created_at AT TIME ZONE 'UTC')::date, (o.failed_at IS NOT NULL), (o.failed_at IS NOT NULL)
      FROM ledger_outbox_events o
     WHERE o.processed_at IS NULL
    UNION ALL
    SELECT 'payment_unposted', u.property_id, u.payment_id::text, u.amount_paise,
           (u.matched_at AT TIME ZONE 'UTC')::date, true, true
      FROM ledger_unposted_payments(p_property) u
     WHERE u.matched_by = 'cashfree'      -- gateway payments must always post; see ADR-016 for cash/manual
    UNION ALL
    SELECT 'payment_posting_orphan', o.property_id, o.source_type || ':' || o.source_id::text, o.amount_paise,
           (o.first_posted AT TIME ZONE 'UTC')::date, true, true
      FROM ledger_orphan_postings(p_property) o
    UNION ALL
    SELECT 'refund_posting_gap', g.property_id, g.refund_id::text, g.gap_paise,
           (g.updated_at AT TIME ZONE 'UTC')::date, true, true
      FROM ledger_refund_posting_gaps(p_property) g
  ), aged AS (
    SELECT r.*, GREATEST(p_as_of - r.originated_on, 0) AS age
      FROM raw r WHERE p_property IS NULL OR r.property_id = p_property
  )
  SELECT a.item_type, a.property_id, a.ref, a.amount_paise, a.originated_on, a.age::int,
         CASE WHEN a.age <= 30 THEN '0-30 current' WHEN a.age <= 60 THEN '31-60 aging'
              WHEN a.age <= 90 THEN '61-90 overdue' ELSE '90+ stale' END,
         CASE WHEN a.is_discrepancy OR a.age > 5 THEN 'investigate' ELSE 'timing' END,
         CASE WHEN a.force_owner OR a.age > 90 OR COALESCE(a.amount_paise, 0) >= 5000000 THEN 'owner'
              WHEN a.age > 60 OR COALESCE(a.amount_paise, 0) >= 1000000 THEN 'manager'
              ELSE 'none' END
    FROM aged a
   ORDER BY a.age DESC, a.amount_paise DESC NULLS LAST;
$$ LANGUAGE sql STABLE;
