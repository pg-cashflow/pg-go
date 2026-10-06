-- Migration 056: drop 10 more redundant btree indexes; make the deposit_settlements RLS policy
-- consistent with the other fail-closed policies (uuid compare, no swallowed errors).
--
-- NUMBERING NOTE: the repo currently has two files numbered 054
-- (054_deposit_settlement_and_universal_outbox.sql and 054_index_dedup_and_autovacuum.sql).
-- The runner keys on the full filename so both apply, but rename the second one to 055_ before
-- it is applied anywhere important (it is idempotent: DROP INDEX IF EXISTS / ALTER ... SET).
--
-- Every index below is a non-unique, non-partial btree whose key columns are a leading prefix of
-- an existing UNIQUE index on the same table, so it adds write cost and cache pressure and no
-- read capability. Found by catalog query on a fresh DB after migrations 001-054.
-- Forward-only runner: to restore one, re-run its original CREATE INDEX.

DROP INDEX IF EXISTS idx_floors_property;                 -- covered by uq_floors_property_floor_number
DROP INDEX IF EXISTS idx_rooms_property;                  -- covered by uq_rooms_property_room_number
DROP INDEX IF EXISTS idx_point_rules_property;            -- covered by uq_point_rules_property_code
DROP INDEX IF EXISTS idx_rewards_catalog_property;        -- covered by uq_rewards_catalog_property_code
DROP INDEX IF EXISTS idx_search_documents_property;       -- covered by uq_search_documents_entity
DROP INDEX IF EXISTS idx_unmatched_provider_payment;      -- covered by uq_unmatched_provider_payment
DROP INDEX IF EXISTS idx_payment_allocations_payment;     -- covered by uq_payment_due_alloc
DROP INDEX IF EXISTS idx_intent_dues_intent;              -- covered by uq_intent_due
DROP INDEX IF EXISTS idx_refunds_payment;                 -- covered by uq_refund_idempotency
DROP INDEX IF EXISTS idx_refund_allocations_refund;       -- covered by uq_refund_due_alloc

-- deposit_settlements policy (054): `current_setting(...) = property_id::text` casts the column,
-- so property_id can never be an index condition, and the surrounding DO block swallowed every
-- error (EXCEPTION WHEN OTHERS THEN NULL), which could leave the table with no policy at all.
-- Re-create it in the same shape as 053 (uuid compare, short-circuiting empty-string guard).
DROP POLICY IF EXISTS deposit_settlements_property_isolation ON deposit_settlements;
CREATE POLICY deposit_settlements_property_isolation ON deposit_settlements
  FOR ALL
  USING (
    property_id = (
      CASE WHEN NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL
           THEN current_setting('app.current_property_id', true)::UUID
           ELSE NULL END
    )
    OR current_setting('app.ledger_maintenance', true) = 'on'
  )
  WITH CHECK (
    property_id = (
      CASE WHEN NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL
           THEN current_setting('app.current_property_id', true)::UUID
           ELSE NULL END
    )
    OR current_setting('app.ledger_maintenance', true) = 'on'
  );
