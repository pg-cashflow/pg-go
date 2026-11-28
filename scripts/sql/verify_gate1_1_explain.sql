-- Gate 1.1 / Gate 2: Verify partial compound index presence and query plan for
-- ListUnbatchedPendingPayoutItemsPaginated.

SELECT indexname, indexdef
FROM pg_indexes
WHERE tablename = 'payout_items'
  AND indexname = 'idx_payout_items_pending_unbatched_cursor';

EXPLAIN (ANALYZE, BUFFERS)
SELECT pi.id, pi.batch_id, pi.payee_id, pi.departure_id, pi.reference_number,
       pi.amount_paise, pi.purpose, pi.period_label, pi.status, pi.utr, pi.settled_at,
       pi.failure_reason, pi.cf_transfer_id, pi.retry_of, pi.created_at, pi.updated_at
FROM payout_items pi
JOIN payout_payees pp ON pp.id = pi.payee_id
WHERE pp.property_id = '00000000-0000-0000-0000-000000000001'::uuid
  AND pi.batch_id IS NULL
  AND pi.status = 'pending'
ORDER BY pi.created_at ASC, pi.id ASC
LIMIT 51;
