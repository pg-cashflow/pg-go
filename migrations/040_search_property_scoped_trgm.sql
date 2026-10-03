-- Migration 040: Property-scoped trigram indexes via btree_gin
-- Restricts trigram index scans to the searched property_id, eliminating
-- cross-property index bloat and multi-property bitmap overhead.

CREATE EXTENSION IF NOT EXISTS btree_gin;

-- 1. Dues: composite (property_id, due_code gin_trgm_ops)
CREATE INDEX IF NOT EXISTS idx_dues_property_due_code_trgm
  ON dues USING gin (property_id, due_code gin_trgm_ops);

-- 2. Bank Transactions: composite (property_id, narration/txn_id gin_trgm_ops)
CREATE INDEX IF NOT EXISTS idx_bank_transactions_prop_narration_trgm
  ON bank_transactions USING gin (property_id, narration gin_trgm_ops);

CREATE INDEX IF NOT EXISTS idx_bank_transactions_prop_txnid_trgm
  ON bank_transactions USING gin (property_id, txn_id gin_trgm_ops);

-- 3. Tenants: composite (property_id, name gin_trgm_ops)
CREATE INDEX IF NOT EXISTS idx_tenants_prop_name_trgm
  ON tenants USING gin (property_id, name gin_trgm_ops);
