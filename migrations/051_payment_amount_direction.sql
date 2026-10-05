-- Migration 051: Payment Amount Direction Invariant
-- Enforce that ordinary collections must have amount > 0,
-- and negative payment amounts are strictly reserved for provider = 'correction' reversals.
-- Applied with NOT VALID so existing databases can reconcile historical rows before validation.

ALTER TABLE payments DROP CONSTRAINT IF EXISTS chk_payments_amount_direction;
ALTER TABLE payments ADD CONSTRAINT chk_payments_amount_direction
  CHECK (amount > 0 OR (provider = 'correction' AND amount < 0)) NOT VALID;
