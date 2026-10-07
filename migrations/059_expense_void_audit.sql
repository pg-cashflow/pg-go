-- Migration 059: expense void audit columns (ADR-021 F1-F3).
-- Voiding an expense now stores who voided it, when, and why.
-- Rows cancelled before this migration (for example, a rejected approval) keep all three NULL.
-- An approval request that is cancelled by a void uses status 'cancelled'
-- (status is VARCHAR(24) with no CHECK constraint, so no change to approval_requests is needed).

ALTER TABLE expenses
  ADD COLUMN IF NOT EXISTS void_reason TEXT,
  ADD COLUMN IF NOT EXISTS voided_by   UUID REFERENCES users(id),
  ADD COLUMN IF NOT EXISTS voided_at   TIMESTAMPTZ;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_expense_void_audit') THEN
    ALTER TABLE expenses ADD CONSTRAINT chk_expense_void_audit CHECK (
      (void_reason IS NULL) = (voided_by IS NULL)
      AND (voided_by IS NULL) = (voided_at IS NULL)
      AND (void_reason IS NULL OR char_length(void_reason) BETWEEN 3 AND 500)
    );
  END IF;
END $$;
