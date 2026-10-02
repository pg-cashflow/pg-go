-- Migration 036: re-run the family_started_at backfill safely.
--
-- Migration 034 originally shipped with a per-row backfill (SET family_started_at = created_at),
-- which restarted the 90-day session ceiling for every session that existed at deploy time.
-- 034 was later corrected in place, which does NOT re-run on databases that had already applied it.
-- This migration is idempotent and only ever moves family_started_at EARLIER, so:
--   * databases that applied the corrected 034: no rows change;
--   * databases that applied the original 034: affected sessions get their real age back,
--     as far as surviving rows allow (rows already purged cannot be recovered).
UPDATE refresh_tokens r
SET family_started_at = f.m
FROM (
    SELECT family_id, MIN(LEAST(created_at, family_started_at)) AS m
    FROM refresh_tokens
    GROUP BY family_id
) f
WHERE r.family_id = f.family_id
  AND r.family_started_at > f.m;
