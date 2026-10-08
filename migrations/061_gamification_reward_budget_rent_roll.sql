-- Migration 061: Gamification F7 Rent-Roll Dynamic Reward Budget and 100-Point Earn Cap
-- Standard: ASD-STE100
-- Method: Empirical First-Principles Verification

-- 1. Add reward budget basis points and ceiling columns to property_gamification_settings
ALTER TABLE property_gamification_settings
  ADD COLUMN IF NOT EXISTS reward_budget_basis_points INT NOT NULL DEFAULT 150,
  ADD COLUMN IF NOT EXISTS reward_budget_ceiling_basis_points INT NOT NULL DEFAULT 200;

-- 2. Lower default earn_cap_per_tenant from 200 to 100 points
ALTER TABLE property_gamification_settings
  ALTER COLUMN earn_cap_per_tenant SET DEFAULT 100;

-- Backfill legacy rows that used old default (200) to new default (100)
UPDATE property_gamification_settings
SET earn_cap_per_tenant = 100
WHERE earn_cap_per_tenant = 200;

-- 3. Add constraint enforcing basis points range and ceiling ordering
ALTER TABLE property_gamification_settings
  DROP CONSTRAINT IF EXISTS chk_gamification_budget_basis_points;

ALTER TABLE property_gamification_settings
  ADD CONSTRAINT chk_gamification_budget_basis_points
  CHECK (
    reward_budget_basis_points > 0 AND
    reward_budget_ceiling_basis_points >= reward_budget_basis_points AND
    reward_budget_ceiling_basis_points <= 1000
  );
