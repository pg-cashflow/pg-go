SET lock_timeout = '5s';

-- Migration 100: Owner-configurable per-refund ceiling for managers.
-- NULL means no ceiling (owner trusts managers with any amount).
-- 0 means managers can never refund and every refund is owner-only.

ALTER TABLE property_onboarding_settings
  ADD COLUMN IF NOT EXISTS manager_refund_limit_paise BIGINT;

ALTER TABLE property_onboarding_settings
  DROP CONSTRAINT IF EXISTS chk_manager_refund_limit_nonneg;
ALTER TABLE property_onboarding_settings
  ADD CONSTRAINT chk_manager_refund_limit_nonneg
  CHECK (manager_refund_limit_paise IS NULL OR manager_refund_limit_paise >= 0);
