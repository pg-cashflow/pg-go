-- Migration 039: Search V2 Tuning & Extended Coverage
-- 1. Drop redundant full-text search indexes (replaced by bare-column trigram substring searches)
DROP INDEX IF EXISTS idx_inspections_notes_fts;
DROP INDEX IF EXISTS idx_hazards_desc_fts;
DROP INDEX IF EXISTS idx_violations_desc_fts;

-- 2. Trigram indexes on operational free-text descriptions, categories, and rule codes
CREATE INDEX IF NOT EXISTS idx_inspections_notes_trgm ON inspections USING gin (notes gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_hazards_desc_trgm ON hazards USING gin (description gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_violations_desc_trgm ON violations USING gin (description gin_trgm_ops);

CREATE INDEX IF NOT EXISTS idx_inspections_type_trgm ON inspections USING gin (inspection_type gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_hazards_category_trgm ON hazards USING gin (category gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_violations_rule_code_trgm ON violations USING gin (rule_code gin_trgm_ops);

-- 3. Trigram indexes for financial search coverage (bank transactions, settlements, payees)
CREATE INDEX IF NOT EXISTS idx_bank_transactions_narration_trgm ON bank_transactions USING gin (narration gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_gateway_settlements_utr_trgm ON gateway_settlements USING gin (utr gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_payout_payees_name_trgm ON payout_payees USING gin (name gin_trgm_ops);
