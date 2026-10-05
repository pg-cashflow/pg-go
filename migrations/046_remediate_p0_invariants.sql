-- Migration 046: Remediate P0/P1 Financial Invariants & Universal BIGINT Paise
--
-- Fulfills:
-- REQ-FIN-001 (P0-01): Uniform 64-bit integer paise across all financial tables (INV-010)
-- REQ-FIN-002 (P0-08): Single-correction invariant per original payment (INV-008)
-- REQ-PAY-003 (P1-11): Mandatory payments.property_id population & NOT NULL constraint (INV-006)
-- REQ-EXP-004 (P1-12): Composite foreign key preventing cross-property room leakage (INV-006)
-- Database semantic check constraints (non-negative balances, valid amounts)

-- ============================================================================
-- 1. MONEY SCHEMA UNIFICATION TO BIGINT (INV-010)
-- ============================================================================

-- Tenants: monthly rent and overpayment carry-forward credit
ALTER TABLE tenants 
  ALTER COLUMN rent_amount TYPE BIGINT,
  ALTER COLUMN credit_balance_paise TYPE BIGINT;

-- Dues: payable amount, contractual original amount, and ceiling
ALTER TABLE dues 
  ALTER COLUMN amount TYPE BIGINT,
  ALTER COLUMN original_amount TYPE BIGINT;

DO $$ 
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns 
    WHERE table_name = 'dues' AND column_name = 'contractual_ceiling_paise'
  ) THEN
    ALTER TABLE dues ALTER COLUMN contractual_ceiling_paise TYPE BIGINT;
  END IF;
END $$;

-- Payments: settled amount
ALTER TABLE payments 
  ALTER COLUMN amount TYPE BIGINT;

-- Payment Reports: tenant reported amount and OCR scanned amount
ALTER TABLE payment_reports 
  ALTER COLUMN amount TYPE BIGINT;

DO $$ 
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.columns 
    WHERE table_name = 'payment_reports' AND column_name = 'ocr_amount'
  ) THEN
    ALTER TABLE payment_reports ALTER COLUMN ocr_amount TYPE BIGINT;
  END IF;
END $$;

-- Payment Intents: gateway intent amount
ALTER TABLE payment_intents 
  ALTER COLUMN amount_paise TYPE BIGINT;

-- ============================================================================
-- 2. FINANCIAL DOMAIN & RANGE CHECK CONSTRAINTS
-- ============================================================================

-- Tenants rent & credit constraints
ALTER TABLE tenants 
  DROP CONSTRAINT IF EXISTS chk_tenants_rent_positive,
  ADD CONSTRAINT chk_tenants_rent_positive CHECK (rent_amount >= 0);

ALTER TABLE tenants 
  DROP CONSTRAINT IF EXISTS chk_tenants_credit_balance,
  ADD CONSTRAINT chk_tenants_credit_balance CHECK (credit_balance_paise >= 0);

-- Dues non-negative payable and positive original amount
ALTER TABLE dues 
  DROP CONSTRAINT IF EXISTS chk_dues_amount_non_negative,
  ADD CONSTRAINT chk_dues_amount_non_negative CHECK (amount >= 0);

ALTER TABLE dues 
  DROP CONSTRAINT IF EXISTS chk_dues_original_positive,
  ADD CONSTRAINT chk_dues_original_positive CHECK (original_amount > 0);

ALTER TABLE dues 
  DROP CONSTRAINT IF EXISTS chk_dues_ceiling_non_negative,
  ADD CONSTRAINT chk_dues_ceiling_non_negative CHECK (contractual_ceiling_paise IS NULL OR contractual_ceiling_paise >= 0);

-- Payments amount cannot be zero (reversals are negative, collections are positive)
ALTER TABLE payments 
  DROP CONSTRAINT IF EXISTS chk_payments_amount_not_zero,
  ADD CONSTRAINT chk_payments_amount_not_zero CHECK (amount != 0);

-- Payment reports & intents must be strictly positive amounts
ALTER TABLE payment_reports 
  DROP CONSTRAINT IF EXISTS chk_payment_reports_amount_positive,
  ADD CONSTRAINT chk_payment_reports_amount_positive CHECK (amount > 0);

ALTER TABLE payment_intents 
  DROP CONSTRAINT IF EXISTS chk_payment_intents_amount_positive,
  ADD CONSTRAINT chk_payment_intents_amount_positive CHECK (amount_paise > 0);

-- ============================================================================
-- 3. FINANCIAL CORRECTION DETERMINISM (INV-008)
-- ============================================================================

-- Enforce exactly one active correction record per original payment
ALTER TABLE financial_corrections 
  DROP CONSTRAINT IF EXISTS uq_financial_corrections_original,
  ADD CONSTRAINT uq_financial_corrections_original UNIQUE (original_payment_id);

-- ============================================================================
-- 4. STRICT PROPERTY SCOPING & INTEGRITY (INV-006)
-- ============================================================================

-- Ensure property_id on payments is strictly populated and NOT NULL
UPDATE payments p
SET property_id = d.property_id
FROM dues d
WHERE d.id = p.due_id AND p.property_id IS NULL;

UPDATE payments p
SET property_id = t.property_id
FROM tenants t
WHERE t.id = p.tenant_id AND p.property_id IS NULL;

-- In case any orphaned payment remains without due/tenant link, guard against NULL
DO $$
BEGIN
  -- Only set NOT NULL if all rows are populated
  IF NOT EXISTS (SELECT 1 FROM payments WHERE property_id IS NULL) THEN
    ALTER TABLE payments ALTER COLUMN property_id SET NOT NULL;
  END IF;
END $$;

-- Enforce composite uniqueness on rooms (id, property_id) to anchor cross-table foreign keys
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'rooms') THEN
    ALTER TABLE rooms 
      DROP CONSTRAINT IF EXISTS uq_rooms_id_property,
      ADD CONSTRAINT uq_rooms_id_property UNIQUE (id, property_id);

    -- Enforce composite FK on expenses (room_id, property_id) -> rooms(id, property_id)
    ALTER TABLE expenses 
      DROP CONSTRAINT IF EXISTS expenses_room_id_fkey,
      DROP CONSTRAINT IF EXISTS fk_expenses_room_property,
      ADD CONSTRAINT fk_expenses_room_property 
        FOREIGN KEY (room_id, property_id) 
        REFERENCES rooms(id, property_id) 
        ON DELETE SET NULL;

    -- Enforce composite FK on tenants (room_id, property_id) -> rooms(id, property_id)
    ALTER TABLE tenants 
      DROP CONSTRAINT IF EXISTS tenants_room_id_fkey,
      DROP CONSTRAINT IF EXISTS fk_tenants_room_property,
      ADD CONSTRAINT fk_tenants_room_property 
        FOREIGN KEY (room_id, property_id) 
        REFERENCES rooms(id, property_id) 
        ON DELETE SET NULL;
  END IF;
END $$;
