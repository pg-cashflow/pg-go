-- Gate 1.1 / Gate 2: Verify partial compound index presence and query plan for
-- ListUnbatchedPendingPayoutItemsPaginated.

BEGIN;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1
    FROM pg_indexes
    WHERE schemaname = 'public'
      AND tablename = 'payout_items'
      AND indexname = 'idx_payout_items_pending_unbatched_cursor'
  ) THEN
    RAISE EXCEPTION 'Missing required index idx_payout_items_pending_unbatched_cursor on payout_items';
  END IF;
END $$;

SELECT indexname, indexdef
FROM pg_indexes
WHERE schemaname = 'public'
  AND tablename = 'payout_items'
  AND indexname = 'idx_payout_items_pending_unbatched_cursor';

-- Seed representative multi-property, multi-payee fixtures inside this rolled-back transaction.
INSERT INTO properties (id, name, owner_phone, upi_vpa, owner_name, owner_email)
VALUES
  ('11111111-1111-4111-8111-111111111111'::uuid, 'Gate2 Explain Property A', '+919900000001', 'gate2a@upi', 'Gate2 Owner A', 'gate2a@example.com'),
  ('22222222-2222-4222-8222-222222222222'::uuid, 'Gate2 Explain Property B', '+919900000002', 'gate2b@upi', 'Gate2 Owner B', 'gate2b@example.com')
ON CONFLICT (id) DO NOTHING;

INSERT INTO payout_payees (id, property_id, payee_type, name, account_number_hash, upi_vpa)
SELECT
  ('33333333-3333-4333-8333-' || lpad(gs::text, 12, '0'))::uuid,
  CASE WHEN gs <= 5 THEN '11111111-1111-4111-8111-111111111111'::uuid ELSE '22222222-2222-4222-8222-222222222222'::uuid END,
  'staff',
  'Gate2 Payee ' || gs,
  repeat(to_hex(gs), 64)::char(64),
  'payee' || gs || '@upi'
FROM generate_series(1, 6) AS gs
ON CONFLICT (id) DO NOTHING;

INSERT INTO payout_items (id, batch_id, payee_id, reference_number, amount_paise, purpose, period_label, status, created_at)
SELECT
  ('44444444-4444-4444-8444-' || lpad(gs::text, 12, '0'))::uuid,
  NULL,
  ('33333333-3333-4333-8333-' || lpad((((gs - 1) % 6) + 1)::text, 12, '0'))::uuid,
  'gate2-ref-' || gs,
  10000 + gs,
  'salary',
  '2025-01',
  CASE
    WHEN gs % 5 = 0 THEN 'succeeded'
    ELSE 'pending'
  END,
  '2025-01-01T00:00:00Z'::timestamptz + (gs * interval '1 second')
FROM generate_series(1, 300) AS gs
ON CONFLICT DO NOTHING;

ANALYZE payout_payees, payout_items;

DO $$
DECLARE
  plan_json json;
BEGIN
  EXECUTE $q$
    EXPLAIN (FORMAT JSON)
    SELECT pi.id, pi.batch_id, pi.payee_id, pi.departure_id, pi.reference_number,
           pi.amount_paise, pi.purpose, pi.period_label, pi.status, pi.utr, pi.settled_at,
           pi.failure_reason, pi.cf_transfer_id, pi.retry_of, pi.created_at, pi.updated_at
    FROM payout_items pi
    JOIN payout_payees pp ON pp.id = pi.payee_id
    WHERE pp.property_id = '11111111-1111-4111-8111-111111111111'::uuid
      AND pi.batch_id IS NULL
      AND pi.status = 'pending'
      AND (pi.created_at, pi.id) > ('2025-01-01T00:01:00Z'::timestamptz, '44444444-4444-4444-8444-000000000060'::uuid)
    ORDER BY pi.created_at ASC, pi.id ASC
    LIMIT 51
  $q$ INTO plan_json;

  IF position('idx_payout_items_pending_unbatched_cursor' IN plan_json::text) = 0 THEN
    RAISE EXCEPTION 'Query plan did not use idx_payout_items_pending_unbatched_cursor: %', plan_json::text;
  END IF;
END $$;

EXPLAIN (ANALYZE, BUFFERS)
SELECT pi.id, pi.batch_id, pi.payee_id, pi.departure_id, pi.reference_number,
       pi.amount_paise, pi.purpose, pi.period_label, pi.status, pi.utr, pi.settled_at,
       pi.failure_reason, pi.cf_transfer_id, pi.retry_of, pi.created_at, pi.updated_at
FROM payout_items pi
JOIN payout_payees pp ON pp.id = pi.payee_id
WHERE pp.property_id = '11111111-1111-4111-8111-111111111111'::uuid
  AND pi.batch_id IS NULL
  AND pi.status = 'pending'
  AND (pi.created_at, pi.id) > ('2025-01-01T00:01:00Z'::timestamptz, '44444444-4444-4444-8444-000000000060'::uuid)
ORDER BY pi.created_at ASC, pi.id ASC
LIMIT 51;

ROLLBACK;
