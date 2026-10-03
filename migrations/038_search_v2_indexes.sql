-- Search V2: Targeted trigram and full-text search indexes on bare columns.
-- Forward-only migration in accordance with D8 and ADR-012.

-- Identifiers and notes
CREATE INDEX IF NOT EXISTS idx_dues_due_code_trgm ON dues USING gin (due_code gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_payments_raw_note_trgm ON payments USING gin (raw_note gin_trgm_ops);

-- Trigram indexes on free-text descriptions and notes for partial/substring search (e.g. "elec" -> "electricity")
CREATE INDEX IF NOT EXISTS idx_inspections_notes_trgm ON inspections USING gin (notes gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_hazards_desc_trgm ON hazards USING gin (description gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_violations_desc_trgm ON violations USING gin (description gin_trgm_ops);

-- Missing trigram indexes on category and code columns
CREATE INDEX IF NOT EXISTS idx_inspections_type_trgm ON inspections USING gin (inspection_type gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_hazards_category_trgm ON hazards USING gin (category gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_violations_rule_code_trgm ON violations USING gin (rule_code gin_trgm_ops);

-- Full-text search indexes using 'simple' configuration (prevents Hinglish / transliteration stemming issues)
CREATE INDEX IF NOT EXISTS idx_inspections_notes_fts ON inspections USING gin (to_tsvector('simple', notes));
CREATE INDEX IF NOT EXISTS idx_hazards_desc_fts ON hazards USING gin (to_tsvector('simple', description));
CREATE INDEX IF NOT EXISTS idx_violations_desc_fts ON violations USING gin (to_tsvector('simple', description));
