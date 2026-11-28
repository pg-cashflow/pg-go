-- Gate 1.1: Partial compound index supporting keyset pagination of unbatched pending payout items
-- ordered by (created_at ASC, id ASC) and joined by payee_id.
CREATE INDEX IF NOT EXISTS idx_payout_items_pending_unbatched_cursor
    ON payout_items (payee_id, created_at ASC, id ASC)
    WHERE batch_id IS NULL AND status = 'pending';
