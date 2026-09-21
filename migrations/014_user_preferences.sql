-- Migration 014: user_preferences table for storing user-level locale and localization settings.
-- Completely additive. user_id references users(id) with CASCADE.

CREATE TABLE IF NOT EXISTS user_preferences (
  user_id    UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  locale     TEXT NOT NULL DEFAULT 'en-IN',
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Enforce supported BCP-47 locale identifiers at DB layer idempotently
DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'chk_user_preferences_locale'
  ) THEN
    ALTER TABLE user_preferences ADD CONSTRAINT chk_user_preferences_locale
      CHECK (locale IN ('en-IN', 'te-IN', 'ta-IN', 'kn-IN'));
  END IF;
END $$;

