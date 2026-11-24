SET lock_timeout = '5s';

-- Migration 125: Enforce FORCE ROW LEVEL SECURITY on platform_payout_reconciliation_cases.
--
-- Migration 124 (platform_payout_reconciliation_cases.sql) enabled RLS with ENABLE ROW LEVEL SECURITY
-- but did not apply FORCE ROW LEVEL SECURITY. Without FORCE, the table owner role can bypass all
-- RLS policies, meaning any connection running as the owner sees all rows regardless of the
-- app.is_platform_ops session variable.
--
-- Migration 122 (refund_approval_requests_rls.sql) correctly uses both ENABLE and FORCE.
-- This migration brings migration-124's table to the same standard.
--
-- Prerequisite: verify this table is not owned by pgapp_app before running.
-- The pgapp_app role must still not be the owner after this migration.
ALTER TABLE platform_payout_reconciliation_cases FORCE ROW LEVEL SECURITY;
