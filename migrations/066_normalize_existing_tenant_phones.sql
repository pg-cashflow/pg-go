-- Migration 066: Normalize existing tenant phone numbers to strict E.164 format.
--
-- Technical Context:
-- Tenants table stores phone numbers in column phone (VARCHAR(15) UNIQUE).
-- Historical records contain raw formats (e.g. 10 digits, leading zero, 12 digits with 91 prefix).
-- This migration standardizes all valid phone numbers to E.164 (+91XXXXXXXXXX or +[10-15 digits]).
--
-- Safety Guarantees:
-- 1. Preflight collision detection aborts the transaction if multiple records produce the same normalized number.
-- 2. Non-conforming or invalid phone numbers remain unchanged with a database NOTICE.
-- 3. Idempotent execution skips records that already conform to normalized E.164 format.
-- 4. Temporary normalization helper function is dropped before transaction completion.

CREATE OR REPLACE FUNCTION normalize_phone_e164(p text)
RETURNS text LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE
    raw text;
    has_plus boolean;
    d text;
BEGIN
    raw := btrim(p, E' \t\r\n');
    IF raw IS NULL OR raw = '' THEN
        RETURN NULL;
    END IF;

    has_plus := (raw LIKE '+%');
    -- Extract digits only
    d := regexp_replace(raw, '[^0-9]', '', 'g');

    IF has_plus THEN
        -- International format: must have 10 to 15 digits
        IF length(d) >= 10 AND length(d) <= 15 THEN
            RETURN '+' || d;
        END IF;
        RETURN NULL;
    END IF;

    -- Leading zero followed by 10 digits (e.g. 09876543210 -> 9876543210)
    IF length(d) = 11 AND d LIKE '0%' THEN
        d := substr(d, 2);
    END IF;

    -- 12 digits starting with country code 91
    IF length(d) = 12 AND d LIKE '91%' THEN
        RETURN '+' || d;
    END IF;

    -- Standard 10-digit Indian mobile number
    IF length(d) = 10 THEN
        RETURN '+91' || d;
    END IF;

    -- Ambiguous or malformed
    RETURN NULL;
END;
$$;

-- Preflight Step 1: Detect unique constraint collisions before update.
DO $$
DECLARE
    collision_count integer;
    collision_list text;
BEGIN
    SELECT count(*), string_agg(norm_phone || ' (count: ' || cnt::text || ')', ', ')
    INTO collision_count, collision_list
    FROM (
        SELECT normalize_phone_e164(phone) AS norm_phone, count(*) AS cnt
        FROM tenants
        WHERE phone IS NOT NULL AND normalize_phone_e164(phone) IS NOT NULL
        GROUP BY normalize_phone_e164(phone)
        HAVING count(*) > 1
    ) sub;

    IF collision_count > 0 THEN
        RAISE EXCEPTION 'Aborting migration 066: phone normalization causes collisions on % numbers: %',
            collision_count, collision_list;
    END IF;
END $$;

-- Preflight Step 2: Log notice for unnormalizable phone numbers.
DO $$
DECLARE
    invalid_count integer;
BEGIN
    SELECT count(*) INTO invalid_count
    FROM tenants
    WHERE phone IS NOT NULL
      AND normalize_phone_e164(phone) IS NULL;

    IF invalid_count > 0 THEN
        RAISE NOTICE 'Migration 066: % tenant phone numbers cannot be normalized to E.164 and remain unchanged',
            invalid_count;
    END IF;
END $$;

-- Step 3: Apply normalization to existing tenant records.
UPDATE tenants
SET phone = normalize_phone_e164(phone)
WHERE phone IS NOT NULL
  AND normalize_phone_e164(phone) IS NOT NULL
  AND length(normalize_phone_e164(phone)) <= 15
  AND phone IS DISTINCT FROM normalize_phone_e164(phone);

-- Step 4: Drop temporary helper function.
DROP FUNCTION IF EXISTS normalize_phone_e164(text);
