-- Search V2: Targeted trigram and full-text search indexes on bare columns.
-- Forward-only migration in accordance with D8 and ADR-012.

CREATE INDEX IF NOT EXISTS idx_dues_due_code_trgm ON dues USING gin (due_code gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_payments_raw_note_trgm ON payments USING gin (raw_note gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_inspections_notes_fts ON inspections USING gin (to_tsvector('english', notes));
CREATE INDEX IF NOT EXISTS idx_hazards_desc_fts ON hazards USING gin (to_tsvector('english', description));
CREATE INDEX IF NOT EXISTS idx_violations_desc_fts ON violations USING gin (to_tsvector('english', description));
