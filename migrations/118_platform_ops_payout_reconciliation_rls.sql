-- Migration 118: Permit cross-property payout batch and exception reconciliation for platform operations
-- Standard: ASD-STE100

-- Allow pgapp_app to query cross-property when app.is_platform_ops = 'true'
DROP POLICY IF EXISTS payout_batches_app ON payout_batches;
CREATE POLICY payout_batches_app ON payout_batches
    FOR ALL TO pgapp_app
    USING (
        current_setting('app.is_platform_ops', true) = 'true'
        OR property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
    )
    WITH CHECK (
        current_setting('app.is_platform_ops', true) = 'true'
        OR property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
    );

DROP POLICY IF EXISTS payout_batches_maint ON payout_batches;
CREATE POLICY payout_batches_maint ON payout_batches
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (
        NULLIF(current_setting('app.current_property_id', true), '') IS NULL
        OR property_id = current_setting('app.current_property_id', true)::uuid
    )
    WITH CHECK (
        NULLIF(current_setting('app.current_property_id', true), '') IS NULL
        OR property_id = current_setting('app.current_property_id', true)::uuid
    );

DROP POLICY IF EXISTS payout_items_app ON payout_items;
CREATE POLICY payout_items_app ON payout_items
    FOR ALL TO pgapp_app
    USING (
        current_setting('app.is_platform_ops', true) = 'true'
        OR EXISTS (
            SELECT 1 FROM payout_payees pp
            WHERE pp.id = payout_items.payee_id
              AND pp.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    )
    WITH CHECK (
        current_setting('app.is_platform_ops', true) = 'true'
        OR EXISTS (
            SELECT 1 FROM payout_payees pp
            WHERE pp.id = payout_items.payee_id
              AND pp.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    );
