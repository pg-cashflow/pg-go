-- Migration 084: Backfill missing manager capabilities.
-- Standard: ASD-STE100
-- Method: Empirical First-Principles Verification
-- ADR-032: Managers must have explicit capability records.
-- Fail-closed authorization removes fallback behavior.
-- This migration grants default delegable capabilities to managers with zero capability rows.

INSERT INTO manager_capabilities (property_id, user_id, capability, granted_by, granted_at, revoked_at)
SELECT
    u.property_id,
    u.id,
    c.capability,
    NULL,
    NOW(),
    NULL
FROM users u
CROSS JOIN (
    VALUES
        ('tenants.view'),
        ('dues.view'),
        ('dues.mark_cash_paid'),
        ('expenses.post'),
        ('expenses.void'),
        ('attendance.mark'),
        ('inspections.write'),
        ('meters.write'),
        ('kitchen.headcount'),
        ('complaints.respond'),
        ('notifications.send'),
        ('slot_invites.create'),
        ('slot_invites.allow_over_capacity')
) AS c(capability)
WHERE u.role = 'manager'
  AND u.property_id IS NOT NULL
  AND NOT EXISTS (
      SELECT 1 FROM manager_capabilities mc
      WHERE mc.property_id = u.property_id
        AND mc.user_id = u.id
  )
ON CONFLICT (property_id, user_id, capability) DO NOTHING;
