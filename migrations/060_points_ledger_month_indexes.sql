-- Migration 060: covering indexes for the monthly gamification cap and budget queries (ADR-021 F6).
-- The queries now filter on an IST month range (created_at >= start AND created_at < end).
-- Both indexes let Postgres answer SUM(delta) without reading the table rows.

CREATE INDEX IF NOT EXISTS idx_points_ledger_tenant_month
  ON points_ledger (tenant_id, created_at) INCLUDE (delta, rule_code);

CREATE INDEX IF NOT EXISTS idx_points_ledger_property_month_cov
  ON points_ledger (property_id, created_at) INCLUDE (delta);

-- The covering index replaces the plain one from migration 006.
DROP INDEX IF EXISTS idx_points_ledger_property_month;
