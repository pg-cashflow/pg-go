-- Migration 035: Enforce append-only immutability for settlement balance runs audit table
-- NOTE: The BEFORE UPDATE OR DELETE trigger on daily_settlement_balance_runs deliberately
-- prevents rows from being modified or deleted. Because properties(id) has ON DELETE CASCADE,
-- attempting to hard-delete a property that has recorded balance runs will also be rejected.
-- This is intentional design to guarantee financial audit trail immutability and compliance.

CREATE OR REPLACE FUNCTION prevent_modification_daily_settlement_balance_runs()
RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'daily_settlement_balance_runs is an immutable append-only audit table';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_prevent_modification ON daily_settlement_balance_runs;
CREATE TRIGGER trg_prevent_modification
BEFORE UPDATE OR DELETE ON daily_settlement_balance_runs
FOR EACH ROW EXECUTE FUNCTION prevent_modification_daily_settlement_balance_runs();
