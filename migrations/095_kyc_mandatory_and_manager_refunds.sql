SET lock_timeout = '5s';

-- Migration 095: Enforce mandatory KYC before pay platform-wide and support delegated manager refunds with owner toggle.

ALTER TABLE property_onboarding_settings
  ALTER COLUMN require_kyc_before_pay SET DEFAULT TRUE;

UPDATE property_onboarding_settings
  SET require_kyc_before_pay = TRUE;

ALTER TABLE property_onboarding_settings
  ADD COLUMN IF NOT EXISTS allow_manager_refunds BOOLEAN NOT NULL DEFAULT FALSE;

ALTER TABLE manager_capabilities DROP CONSTRAINT IF EXISTS manager_capabilities_delegable;
ALTER TABLE manager_capabilities ADD CONSTRAINT manager_capabilities_delegable CHECK (capability IN (
    'tenants.view',
    'dues.view',
    'dues.mark_cash_paid',
    'expenses.post',
    'expenses.void',
    'attendance.mark',
    'inspections.write',
    'meters.write',
    'kitchen.headcount',
    'complaints.respond',
    'notifications.send',
    'slot_invites.create',
    'slot_invites.allow_over_capacity',
    'refunds.issue'
));

