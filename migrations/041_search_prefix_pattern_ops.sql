-- Migration 041: Prefix pattern-ops indexes & redundant index cleanup
-- Enables fast B-Tree prefix range seeks (~>=~ and ~<~) for code-like identifiers,
-- bypassing costly GIN trigram scans for prefix lookups.

-- 1. Payments: prefix index on lower(upi_txn_id)
CREATE INDEX IF NOT EXISTS idx_payments_upi_prefix
  ON payments (lower(upi_txn_id) text_pattern_ops);

-- 2. Bank Transactions: property-scoped prefix index on lower(txn_id)
CREATE INDEX IF NOT EXISTS idx_bank_txn_prop_prefix
  ON bank_transactions (property_id, lower(txn_id) text_pattern_ops);

-- 3. Cleanup: drop redundant composite (property_id, name) GIN index on tenants
-- The btree on property_id already covers property scoping, so this GIN index was redundant.
DROP INDEX IF EXISTS idx_tenants_prop_name_trgm;
