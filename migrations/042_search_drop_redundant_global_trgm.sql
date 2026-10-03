-- Migration 042: drop global trigram indexes superseded by property-scoped composites (040)
DROP INDEX IF EXISTS idx_bank_transactions_narration_trgm;  -- 039, superseded by idx_bank_transactions_prop_narration_trgm
DROP INDEX IF EXISTS idx_bank_transactions_txnid_trgm;      -- 039, superseded by idx_bank_transactions_prop_txnid_trgm
-- Keep idx_dues_due_code_trgm until plans are checked at several property sizes.
