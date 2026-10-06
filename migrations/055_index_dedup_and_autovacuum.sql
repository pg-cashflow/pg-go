-- Migration 055: remove redundant indexes; tune autovacuum on high-churn tables.
--
-- Rationale (see docs/adr/018-rls-scope-wiring-and-db-performance-hardening.md):
--   Every index is paid for on every INSERT/UPDATE of its table. The indexes below add
--   no read capability because an equivalent (or strictly wider) index already exists.
--   Each was verified by scanning all migrations: the covering index is non-partial and
--   has not been dropped by a later migration.
--
-- Safety:
--   * Plain DROP INDEX IF EXISTS (idempotent). Takes a brief ACCESS EXCLUSIVE lock; the
--     tables involved are small today. On a large production table use
--     DROP INDEX CONCURRENTLY outside a transaction instead.
--   * No application SQL references these index names (grepped *.go / *.sql).
--   * Forward-only runner: to restore any index, re-run its original CREATE INDEX
--     (file noted beside each statement).

-- A. Redundant with an inline UNIQUE constraint (a UNIQUE constraint already owns an index).
DROP INDEX IF EXISTS idx_dues_due_code;                         -- 001: dues.due_code UNIQUE
DROP INDEX IF EXISTS idx_refresh_tokens_hash;                   -- 030: refresh_tokens.token_hash UNIQUE
DROP INDEX IF EXISTS idx_calendar_tokens_hash;                  -- 048: property_calendar_tokens.token_hash UNIQUE

-- B. Left-prefix of a wider btree on the same table (wider index serves the same lookups).
DROP INDEX IF EXISTS idx_dues_property_tenant;                  -- 039: (property_id, tenant_id) ⊂ idx_dues_prop_tenant_status (045)
DROP INDEX IF EXISTS idx_expenses_property_occurred;            -- 012: (property_id, occurred_at) ⊂ idx_expenses_prop_occurred_cat (045)
DROP INDEX IF EXISTS idx_gateway_settlements_cf_settlement;     -- 026: (cf_settlement_id) ⊂ uq_gateway_settlement_record
DROP INDEX IF EXISTS idx_refresh_tokens_family;                 -- 030: (family_id) ⊂ idx_refresh_tokens_family_revoked / _family_started

-- C. Autovacuum tuning for tables with constant insert/update/delete churn.
--    Defaults (scale_factor 0.2) wait until ~20% of the table is dead before vacuuming,
--    which lets queue-like tables bloat and degrades their "pending" partial indexes.
--    Guarded with to_regclass so the migration cannot fail if a table is absent.
DO $$
DECLARE
  t TEXT;
BEGIN
  FOREACH t IN ARRAY ARRAY[
    'outbox_events',
    'ledger_outbox_events',
    'notification_deliveries',
    'otp_requests',
    'refresh_tokens',
    'payment_tokens'
  ]
  LOOP
    IF to_regclass('public.' || t) IS NOT NULL THEN
      EXECUTE format(
        'ALTER TABLE %I SET (
           autovacuum_vacuum_scale_factor  = 0.02,
           autovacuum_analyze_scale_factor = 0.02,
           autovacuum_vacuum_threshold     = 50,
           autovacuum_analyze_threshold    = 50
         )', t);
    END IF;
  END LOOP;
END
$$;
