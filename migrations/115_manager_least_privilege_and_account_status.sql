SET lock_timeout = '5s';

-- Migration 115: Manager Least Privilege, Account Status, and Transactional Audit Outbox
-- Standard: ASD-STE100
-- Purpose:
--   1. Enforce explicit account lifecycle state with disabled_at and profile columns.
--   2. Restrict manager capabilities to 14 delegable items.
--   3. Migrate legacy refunds.issue assignments to refunds.request.
--   4. Establish transactional security audit outbox table.

-- Step 1: Add account status and profile columns to users table
ALTER TABLE users 
  ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS profile VARCHAR(30) NOT NULL DEFAULT 'custom';

-- Enforce allowed profile values
ALTER TABLE users DROP CONSTRAINT IF EXISTS chk_user_profile;
ALTER TABLE users ADD CONSTRAINT chk_user_profile 
  CHECK (profile IN ('front_desk', 'operations', 'finance_assistant', 'custom'));

-- Step 2: Backward-compatible capability data migration
-- Migrate legacy refunds.issue capability to refunds.request
UPDATE manager_capabilities 
  SET capability = 'refunds.request' 
  WHERE capability = 'refunds.issue';

-- Remove any non-delegable or deprecated capabilities before applying the new constraint
DELETE FROM manager_capabilities 
  WHERE capability NOT IN (
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
    'refunds.request'
  );

-- Step 3: Install the updated capability check constraint
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
    'refunds.request'
));

-- Step 4: Create transactional security audit outbox table
CREATE TABLE IF NOT EXISTS security_events_outbox (
    id BIGSERIAL PRIMARY KEY,
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    actor_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    target_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    event_type VARCHAR(64) NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    status VARCHAR(20) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'dispatched', 'failed')),
    attempts INT NOT NULL DEFAULT 0,
    last_error TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    dispatched_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_security_events_outbox_pending 
    ON security_events_outbox(id) WHERE status = 'pending';
