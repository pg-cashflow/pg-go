-- Migration 141: Separate Cashfree payment identity (cf_payment_id) from bank_reference and legacy manual UTRs (upi_txn_id).
-- Fixes F1: A shared bank_reference across distinct Cashfree payments must never collide on upi_txn_id.

SET LOCAL lock_timeout = '5s';

-- 1. Preflight safety checks: abort loudly if existing data violates the target invariants.
DO $$
BEGIN
  IF EXISTS (
    SELECT 1
    FROM payments
    WHERE cf_payment_id IS NOT NULL
    GROUP BY cf_payment_id
    HAVING COUNT(*) > 1
  ) THEN
    RAISE EXCEPTION 'migration 141 aborted: duplicate cf_payment_id values exist in payments';
  END IF;

  IF EXISTS (
    SELECT 1
    FROM payments
    WHERE upi_txn_id IS NOT NULL
      AND BTRIM(upi_txn_id) <> ''
      AND matched_by <> 'cashfree'
      AND provider <> 'cashfree'
    GROUP BY UPPER(BTRIM(upi_txn_id))
    HAVING COUNT(*) > 1
  ) THEN
    RAISE EXCEPTION 'migration 141 aborted: duplicate case-insensitive legacy upi_txn_id values exist in payments';
  END IF;
END $$;

-- 2. Add nullable non-unique bank_reference column.
ALTER TABLE payments
  ADD COLUMN IF NOT EXISTS bank_reference VARCHAR(100);

-- 3. Backfill historical Cashfree bank references from upi_txn_id only when distinct from cf_payment_id.
UPDATE payments
SET bank_reference = BTRIM(upi_txn_id)
WHERE (provider = 'cashfree' OR matched_by = 'cashfree')
  AND bank_reference IS NULL
  AND upi_txn_id IS NOT NULL
  AND BTRIM(upi_txn_id) <> ''
  AND (cf_payment_id IS NULL OR BTRIM(upi_txn_id) <> cf_payment_id);

-- 4. Clear misused Cashfree values from upi_txn_id.
UPDATE payments
SET upi_txn_id = NULL
WHERE (provider = 'cashfree' OR matched_by = 'cashfree')
  AND upi_txn_id IS NOT NULL;

-- 5. Drop legacy global UNIQUE constraint(s) on payments.upi_txn_id.
DO $$
DECLARE
  con_rec RECORD;
BEGIN
  FOR con_rec IN
    SELECT c.conname
    FROM pg_constraint c
    JOIN pg_class t ON t.oid = c.conrelid
    JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = ANY(c.conkey)
    WHERE t.relname = 'payments'
      AND c.contype = 'u'
      AND array_length(c.conkey, 1) = 1
      AND a.attname = 'upi_txn_id'
  LOOP
    EXECUTE format('ALTER TABLE payments DROP CONSTRAINT IF EXISTS %I', con_rec.conname);
  END LOOP;
END $$;
DROP INDEX IF EXISTS payments_upi_txn_id_key;

-- 6. Create conditional partial UNIQUE index for non-Cashfree legacy/manual/bank UTRs.
CREATE UNIQUE INDEX IF NOT EXISTS uq_payments_legacy_utr
  ON payments (UPPER(upi_txn_id))
  WHERE upi_txn_id IS NOT NULL
    AND matched_by <> 'cashfree'
    AND provider <> 'cashfree';

-- 7. Standardize canonical Cashfree unique constraint name to uq_payments_cf_payment_id.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conrelid = 'payments'::regclass AND conname = 'uq_payments_cf_payment_id'
  ) THEN
    IF EXISTS (
      SELECT 1 FROM pg_constraint
      WHERE conrelid = 'payments'::regclass AND conname = 'payments_cf_payment_id_key'
    ) THEN
      ALTER TABLE payments RENAME CONSTRAINT payments_cf_payment_id_key TO uq_payments_cf_payment_id;
    ELSE
      ALTER TABLE payments ADD CONSTRAINT uq_payments_cf_payment_id UNIQUE (cf_payment_id);
    END IF;
  END IF;
END $$;

-- 8. Scope provider_payment_id uniqueness to non-Cashfree providers so Cashfree has one canonical constraint (uq_payments_cf_payment_id).
ALTER TABLE payments DROP CONSTRAINT IF EXISTS uq_payments_provider_payment;
DROP INDEX IF EXISTS uq_payments_provider_payment;
CREATE UNIQUE INDEX IF NOT EXISTS uq_payments_non_cashfree_provider_payment
  ON payments (provider, provider_payment_id)
  WHERE provider <> 'cashfree' AND provider_payment_id IS NOT NULL;

-- 9. Enforce that Cashfree payments cannot populate upi_txn_id going forward.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conrelid = 'payments'::regclass AND conname = 'chk_payments_cashfree_no_upi_txn_id'
  ) THEN
    ALTER TABLE payments
      ADD CONSTRAINT chk_payments_cashfree_no_upi_txn_id
      CHECK ((provider <> 'cashfree' AND matched_by <> 'cashfree') OR upi_txn_id IS NULL);
  END IF;
END $$;
