-- Migration 045: Financial integrity, immutable correction audit, indexes, and daily rollups
--
-- Fulfills PG Cashflow Backend Requirements:
-- 1. Money data accuracy & integer paise
-- 2. Unique UTR & property scoping
-- 3. Indexes for dues, payments, expenses
-- 4. Immutable financial corrections audit trail
-- 5. Daily financial rollups
-- 6. Recurring expenses support

-- 1. Payments: Ensure property_id column and property-scoped compound indexes exist
ALTER TABLE payments ADD COLUMN IF NOT EXISTS property_id UUID REFERENCES properties(id);

-- Backfill property_id from dues or tenants for existing rows
UPDATE payments p
SET property_id = d.property_id
FROM dues d
WHERE d.id = p.due_id AND p.property_id IS NULL;

UPDATE payments p
SET property_id = t.property_id
FROM tenants t
WHERE t.id = p.tenant_id AND p.property_id IS NULL;

-- Payment Indexes: property_id, date, UTR
CREATE INDEX IF NOT EXISTS idx_payments_prop_date
  ON payments (property_id, matched_at DESC);

CREATE INDEX IF NOT EXISTS idx_payments_prop_utr
  ON payments (property_id, UPPER(upi_txn_id))
  WHERE upi_txn_id IS NOT NULL;

-- 2. Dues Indexes: property_id, tenant_id, status, due_date
CREATE INDEX IF NOT EXISTS idx_dues_prop_tenant_status
  ON dues (property_id, tenant_id, status);

CREATE INDEX IF NOT EXISTS idx_dues_prop_due_date
  ON dues (property_id, due_date);

-- 3. Expenses: Add recurring indicator and compound indexes
ALTER TABLE expenses ADD COLUMN IF NOT EXISTS is_recurring BOOLEAN NOT NULL DEFAULT FALSE;

CREATE INDEX IF NOT EXISTS idx_expenses_prop_occurred_cat
  ON expenses (property_id, occurred_at, category_code);

CREATE INDEX IF NOT EXISTS idx_expenses_prop_cat
  ON expenses (property_id, category_code);

-- Insert recurring expense categories (gas, security, utilities) if missing
INSERT INTO financial_categories (property_id, code, name, cost_behavior, controllability, purpose, is_system)
VALUES
  (NULL, 'gas', 'Gas', 'variable', 'partial', 'operations', true),
  (NULL, 'security', 'Security', 'fixed', 'partial', 'operations', true),
  (NULL, 'utilities', 'Other utilities', 'variable', 'partial', 'operations', true)
ON CONFLICT DO NOTHING;

-- 4. Daily Financial Rollups (Property-level daily summary for Owner dashboard historical charts)
CREATE TABLE IF NOT EXISTS daily_financial_rollups (
  id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id         UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  rollup_date         DATE NOT NULL,
  collected_paise     BIGINT NOT NULL DEFAULT 0,
  due_paise           BIGINT NOT NULL DEFAULT 0,
  expense_paise       BIGINT NOT NULL DEFAULT 0,
  net_cash_flow_paise BIGINT NOT NULL DEFAULT 0,
  total_rooms         INT NOT NULL DEFAULT 0,
  occupied_rooms      INT NOT NULL DEFAULT 0,
  capacity_beds       INT NOT NULL DEFAULT 0,
  occupied_beds       INT NOT NULL DEFAULT 0,
  occupancy_rate_pct  NUMERIC(5, 2) NOT NULL DEFAULT 0.00,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_daily_financial_rollups_prop_date UNIQUE (property_id, rollup_date)
);

CREATE INDEX IF NOT EXISTS idx_daily_financial_rollups_prop_date
  ON daily_financial_rollups (property_id, rollup_date DESC);

-- 5. Immutable Financial Audit Trail for Corrections
CREATE TABLE IF NOT EXISTS financial_corrections (
  id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id          UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  original_payment_id  UUID NOT NULL REFERENCES payments(id),
  reversal_payment_id  UUID REFERENCES payments(id),
  corrected_payment_id UUID REFERENCES payments(id),
  reason               TEXT NOT NULL,
  corrected_by         UUID NOT NULL REFERENCES users(id),
  occurred_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_financial_corrections_prop
  ON financial_corrections (property_id, occurred_at DESC);

CREATE OR REPLACE FUNCTION trg_financial_corrections_immutable() RETURNS trigger AS $$
BEGIN
  RAISE EXCEPTION 'financial_corrections is an immutable audit log (% blocked)', TG_OP
    USING ERRCODE = 'LG003';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_financial_corrections_immutable ON financial_corrections;
CREATE TRIGGER trg_financial_corrections_immutable
  BEFORE UPDATE OR DELETE ON financial_corrections
  FOR EACH ROW EXECUTE FUNCTION trg_financial_corrections_immutable();
