-- Migration 043: ledger integrity controls (ADR-015)
-- C-1  append-only journal  – rows may not be UPDATEd; DELETEs only in maintenance mode.
-- C-2  DB-enforced double entry – every (source_type, source_id) must balance at COMMIT.
-- C-3  closed periods are frozen – reopening requires an explicit, flagged act.
--
-- Escape hatch for tests/maintenance only:
--   SET LOCAL app.ledger_maintenance = 'on';   -- enables DELETEs on the journal
--   SET LOCAL app.reopen_period     = 'on';   -- allows updating a closed period_tie_out

-- ─────────────────────────────────────────────────────────────────────────────
-- C-1: financial_journal_entries is append-only.
--      UPDATEs are always blocked (use a reversing entry instead).
--      DELETEs are blocked unless app.ledger_maintenance = 'on'.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION trg_journal_append_only() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'UPDATE' THEN
    RAISE EXCEPTION
      'financial_journal_entries is append-only (UPDATE blocked); '
      'post a reversing entry instead';
  END IF;
  -- TG_OP = 'DELETE'
  IF COALESCE(current_setting('app.ledger_maintenance', true), 'off') <> 'on' THEN
    RAISE EXCEPTION 'financial_journal_entries is append-only (DELETE blocked)';
  END IF;
  RETURN OLD;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_journal_append_only ON financial_journal_entries;
CREATE TRIGGER trg_journal_append_only
  BEFORE UPDATE OR DELETE ON financial_journal_entries
  FOR EACH ROW EXECUTE FUNCTION trg_journal_append_only();

-- ─────────────────────────────────────────────────────────────────────────────
-- C-2: every (source_type, source_id) must have equal total debits and credits
--      at transaction COMMIT time.  Constraint trigger fires AFTER INSERT,
--      DEFERRABLE INITIALLY DEFERRED so all lines for a source can be inserted
--      within the same transaction before the check runs.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION trg_journal_balanced() RETURNS trigger AS $$
DECLARE
  v_debit  bigint;
  v_credit bigint;
BEGIN
  SELECT
    COALESCE(SUM(debit_paise),  0),
    COALESCE(SUM(credit_paise), 0)
  INTO v_debit, v_credit
  FROM financial_journal_entries
  WHERE source_type = NEW.source_type
    AND source_id   = NEW.source_id;

  IF v_debit <> v_credit THEN
    RAISE EXCEPTION
      'unbalanced journal for %/%: debit=% credit=%',
      NEW.source_type, NEW.source_id, v_debit, v_credit;
  END IF;
  RETURN NULL;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_journal_balanced ON financial_journal_entries;
CREATE CONSTRAINT TRIGGER trg_journal_balanced
  AFTER INSERT ON financial_journal_entries
  DEFERRABLE INITIALLY DEFERRED
  FOR EACH ROW EXECUTE FUNCTION trg_journal_balanced();

-- ─────────────────────────────────────────────────────────────────────────────
-- C-3: period_tie_outs rows with status = 'closed' cannot be modified or deleted
--      without the appropriate escape-hatch GUC.
--      Reopening a period requires:  SET LOCAL app.reopen_period = 'on';
--      Deleting a closed period requires: SET LOCAL app.ledger_maintenance = 'on';
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION trg_tieout_closed_frozen() RETURNS trigger AS $$
BEGIN
  IF TG_OP = 'DELETE' THEN
    IF OLD.status = 'closed'
       AND COALESCE(current_setting('app.ledger_maintenance', true), 'off') <> 'on'
    THEN
      RAISE EXCEPTION 'closed period % cannot be deleted', OLD.period_month;
    END IF;
    RETURN OLD;
  END IF;
  -- TG_OP = 'UPDATE'
  IF OLD.status = 'closed'
     AND COALESCE(current_setting('app.reopen_period', true), 'off') <> 'on'
  THEN
    RAISE EXCEPTION
      'period % is closed; reopen requires SET LOCAL app.reopen_period = ''on''',
      OLD.period_month;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_tieout_closed_frozen ON period_tie_outs;
CREATE TRIGGER trg_tieout_closed_frozen
  BEFORE UPDATE OR DELETE ON period_tie_outs
  FOR EACH ROW EXECUTE FUNCTION trg_tieout_closed_frozen();
