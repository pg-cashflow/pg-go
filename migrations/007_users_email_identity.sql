-- Migration 007: Allow email-based identity alongside phone for users.
--
-- PRE-FLIGHT VERIFICATION (Run in production before applying this migration):
-- To guarantee that the E.164 phone backfill UPDATE does not trigger collisions against
-- the existing UNIQUE constraints on `users.phone` and `tenants.phone`, run these checks:
--
-- 1. Check for phone collisions in `users`:
--    SELECT
--      CASE
--        WHEN length(regexp_replace(phone, '\D', '', 'g')) = 10
--          THEN '+91' || regexp_replace(phone, '\D', '', 'g')
--        WHEN length(regexp_replace(phone, '\D', '', 'g')) = 11 AND regexp_replace(phone, '\D', '', 'g') LIKE '0%'
--          THEN '+91' || substr(regexp_replace(phone, '\D', '', 'g'), 2)
--        WHEN phone LIKE '+%'
--          THEN '+' || regexp_replace(phone, '\D', '', 'g')
--        ELSE phone
--      END AS normalized_phone,
--      COUNT(*) AS occurrences,
--      array_agg(id) AS user_ids,
--      array_agg(phone) AS original_phones
--    FROM users
--    WHERE phone IS NOT NULL AND phone != ''
--    GROUP BY 1 HAVING COUNT(*) > 1;
--
-- 2. Check for phone collisions in `tenants`:
--    SELECT
--      CASE
--        WHEN length(regexp_replace(phone, '\D', '', 'g')) = 10
--          THEN '+91' || regexp_replace(phone, '\D', '', 'g')
--        WHEN length(regexp_replace(phone, '\D', '', 'g')) = 11 AND regexp_replace(phone, '\D', '', 'g') LIKE '0%'
--          THEN '+91' || substr(regexp_replace(phone, '\D', '', 'g'), 2)
--        WHEN phone LIKE '+%'
--          THEN '+' || regexp_replace(phone, '\D', '', 'g')
--        ELSE phone
--      END AS normalized_phone,
--      COUNT(*) AS occurrences,
--      array_agg(id) AS tenant_ids,
--      array_agg(phone) AS original_phones
--    FROM tenants
--    WHERE phone IS NOT NULL AND phone != ''
--    GROUP BY 1 HAVING COUNT(*) > 1;
--
-- If either query returns rows, reconcile duplicates prior to migration deployment.

ALTER TABLE users ALTER COLUMN phone DROP NOT NULL;
ALTER TABLE users ADD COLUMN email VARCHAR(255);

ALTER TABLE users ADD CONSTRAINT chk_user_identity
  CHECK (phone IS NOT NULL OR email IS NOT NULL);

CREATE UNIQUE INDEX idx_users_email_ci
  ON users (LOWER(email))
  WHERE email IS NOT NULL;

-- Data Backfill: Normalize existing legacy phone numbers across properties, users, and tenants to E.164.
-- Converts 10-digit Indian numbers ("9876543210") or 0-prefixed 11-digit numbers ("09876543210") to "+919876543210".
UPDATE properties
SET owner_phone = CASE
  WHEN length(regexp_replace(owner_phone, '\D', '', 'g')) = 10
    THEN '+91' || regexp_replace(owner_phone, '\D', '', 'g')
  WHEN length(regexp_replace(owner_phone, '\D', '', 'g')) = 11 AND regexp_replace(owner_phone, '\D', '', 'g') LIKE '0%'
    THEN '+91' || substr(regexp_replace(owner_phone, '\D', '', 'g'), 2)
  WHEN owner_phone LIKE '+%'
    THEN '+' || regexp_replace(owner_phone, '\D', '', 'g')
  ELSE owner_phone
END
WHERE owner_phone IS NOT NULL AND owner_phone != '';

UPDATE users
SET phone = CASE
  WHEN length(regexp_replace(phone, '\D', '', 'g')) = 10
    THEN '+91' || regexp_replace(phone, '\D', '', 'g')
  WHEN length(regexp_replace(phone, '\D', '', 'g')) = 11 AND regexp_replace(phone, '\D', '', 'g') LIKE '0%'
    THEN '+91' || substr(regexp_replace(phone, '\D', '', 'g'), 2)
  WHEN phone LIKE '+%'
    THEN '+' || regexp_replace(phone, '\D', '', 'g')
  ELSE phone
END
WHERE phone IS NOT NULL AND phone != '';

UPDATE tenants
SET phone = CASE
  WHEN length(regexp_replace(phone, '\D', '', 'g')) = 10
    THEN '+91' || regexp_replace(phone, '\D', '', 'g')
  WHEN length(regexp_replace(phone, '\D', '', 'g')) = 11 AND regexp_replace(phone, '\D', '', 'g') LIKE '0%'
    THEN '+91' || substr(regexp_replace(phone, '\D', '', 'g'), 2)
  WHEN phone LIKE '+%'
    THEN '+' || regexp_replace(phone, '\D', '', 'g')
  ELSE phone
END
WHERE phone IS NOT NULL AND phone != '';

-- Rollback (Down Migration Reference):
-- DROP INDEX IF EXISTS idx_users_email_ci;
-- ALTER TABLE users DROP CONSTRAINT IF EXISTS chk_user_identity;
-- ALTER TABLE users DROP COLUMN IF EXISTS email;
-- ALTER TABLE users ALTER COLUMN phone SET NOT NULL;
