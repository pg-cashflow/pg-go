-- Read-only preflight check before applying migration 141_cashfree_payment_identity_separation.sql.
-- Both queries must return 0 rows before running migration 141 in production.

-- 1. Duplicate cf_payment_id values
SELECT cf_payment_id, COUNT(*) AS row_count, array_agg(id ORDER BY created_at) AS payment_ids
FROM payments
WHERE cf_payment_id IS NOT NULL
GROUP BY cf_payment_id
HAVING COUNT(*) > 1;

-- 2. Duplicate case-insensitive legacy/manual/bank upi_txn_id values
SELECT UPPER(BTRIM(upi_txn_id)) AS normalized_utr, COUNT(*) AS row_count, array_agg(id ORDER BY created_at) AS payment_ids
FROM payments
WHERE upi_txn_id IS NOT NULL
  AND BTRIM(upi_txn_id) <> ''
  AND matched_by <> 'cashfree'
  AND provider <> 'cashfree'
GROUP BY UPPER(BTRIM(upi_txn_id))
HAVING COUNT(*) > 1;
