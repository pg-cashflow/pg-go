SET lock_timeout = '5s';

-- Migration 121: Permit platform operations to read payee labels for payout exception reconciliation.
-- Keep this as SELECT only. Existing property-scoped policies continue to govern normal app access.
DROP POLICY IF EXISTS payout_payees_platform_ops_select ON payout_payees;
CREATE POLICY payout_payees_platform_ops_select ON payout_payees
    FOR SELECT TO pgapp_app
    USING (current_setting('app.is_platform_ops', true) = 'true');
