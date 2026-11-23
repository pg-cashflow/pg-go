SET lock_timeout = '5s';

-- Migration 122: Enforce property isolation for refund approval requests.
-- Application access requires a verified transaction-local property scope.
-- Trusted maintenance/worker roles retain service access through separate policies.

ALTER TABLE refund_approval_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE refund_approval_requests FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS refund_approval_requests_app ON refund_approval_requests;
CREATE POLICY refund_approval_requests_app ON refund_approval_requests
    FOR ALL TO pgapp_app
    USING (
        property_id = CASE
            WHEN NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL
            THEN current_setting('app.current_property_id', true)::uuid
            ELSE NULL
        END
    )
    WITH CHECK (
        property_id = CASE
            WHEN NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL
            THEN current_setting('app.current_property_id', true)::uuid
            ELSE NULL
        END
    );

DROP POLICY IF EXISTS refund_approval_requests_service ON refund_approval_requests;
CREATE POLICY refund_approval_requests_service ON refund_approval_requests
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE, DELETE
    ON refund_approval_requests TO pgapp_app, pgapp_maint, pgapp_worker;
