-- Migration 059: Expense Void Audit Columns and Gamification Points Ledger Range Index

-- 1. Add audit columns to expenses table
ALTER TABLE expenses
  ADD COLUMN IF NOT EXISTS void_reason TEXT,
  ADD COLUMN IF NOT EXISTS voided_by UUID REFERENCES users(id),
  ADD COLUMN IF NOT EXISTS voided_at TIMESTAMPTZ;

-- 2. Enforce audit trail integrity when an expense is cancelled
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'chk_expenses_void_audit'
  ) THEN
    ALTER TABLE expenses
      ADD CONSTRAINT chk_expenses_void_audit
      CHECK (status != 'cancelled' OR (void_reason IS NOT NULL AND voided_by IS NOT NULL AND voided_at IS NOT NULL));
  END IF;
END $$;

-- 3. Add composite range index on points_ledger for tenant monthly lookups (F6)
CREATE INDEX IF NOT EXISTS idx_points_ledger_tenant_created
  ON points_ledger (tenant_id, created_at);
