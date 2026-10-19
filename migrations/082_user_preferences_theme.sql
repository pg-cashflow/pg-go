-- Migration 082: Add nullable theme to user_preferences.
-- NULL indicates the user has not explicitly saved a theme.
SET lock_timeout = '2s';

ALTER TABLE user_preferences
  ADD COLUMN IF NOT EXISTS theme TEXT DEFAULT NULL;

DO $$ BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint WHERE conname = 'chk_user_preferences_theme'
  ) THEN
    ALTER TABLE user_preferences ADD CONSTRAINT chk_user_preferences_theme
      CHECK (theme IS NULL OR theme IN ('system', 'light', 'dark'));
  END IF;
END $$;
