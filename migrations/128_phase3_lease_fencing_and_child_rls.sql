SET lock_timeout = '5s';

-- Migration 128: Phase 3 Database Integrity, Lease Fencing & Child Tables RLS
-- Standard: ASD-STE100
-- Purpose:
--   1. Reconcile preflight duplicate active KYC check without modifying immutable migration 126.
--   2. Add canonical expires_at column to kyc_in_flight_lock.
--   3. Retain durable payout reconciliation incidents with ON DELETE SET NULL on batch_id and payout_item_id.
--   4. Split financial_categories application policies to allow global category reads while strictly
--      forbidding modification or deletion of global rows (property_id IS NULL).
--   5. Enforce fail-closed parent-derived RLS on payment, refund, notification, and gamification child tables.

-- ============================================================================
-- 1. Preflight Duplicate KYC Active States Check
-- ============================================================================
DO $$
DECLARE
    dup_count INT;
    dup_tenants TEXT;
BEGIN
    SELECT COUNT(*), string_agg(tenant_id::text, ', ')
    INTO dup_count, dup_tenants
    FROM (
        SELECT tenant_id
        FROM kyc_verification
        WHERE status IN ('pending', 'request_unknown', 'pending_manual_review', 'verified')
        GROUP BY tenant_id
        HAVING COUNT(*) > 1
    ) dups;

    IF dup_count > 0 THEN
        RAISE EXCEPTION 'Migration 128 preflight failed: detected % tenant(s) with multiple active KYC verifications (tenants: %). Reconcile before applying index backstop.', dup_count, dup_tenants;
    END IF;
END $$;

-- ============================================================================
-- 2. KYC In-Flight Lease Fencing Schema (Canonical expires_at)
-- ============================================================================
ALTER TABLE kyc_in_flight_lock 
    ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ NOT NULL DEFAULT (NOW() + INTERVAL '5 minutes');

CREATE INDEX IF NOT EXISTS idx_kyc_in_flight_lock_expiry ON kyc_in_flight_lock(expires_at);

-- ============================================================================
-- 3. Durable Payout Incident Foreign Keys (ON DELETE SET NULL)
-- ============================================================================
ALTER TABLE payout_reconciliation_incidents
    ALTER COLUMN batch_id DROP NOT NULL,
    ALTER COLUMN payout_item_id DROP NOT NULL;

ALTER TABLE payout_reconciliation_incidents
    DROP CONSTRAINT IF EXISTS payout_reconciliation_incidents_batch_id_fkey,
    DROP CONSTRAINT IF EXISTS payout_reconciliation_incidents_payout_item_id_fkey;

ALTER TABLE payout_reconciliation_incidents
    ADD CONSTRAINT payout_reconciliation_incidents_batch_id_fkey
        FOREIGN KEY (batch_id) REFERENCES payout_batches(id) ON DELETE SET NULL,
    ADD CONSTRAINT payout_reconciliation_incidents_payout_item_id_fkey
        FOREIGN KEY (payout_item_id) REFERENCES payout_items(id) ON DELETE SET NULL;

-- ============================================================================
-- 4. Split financial_categories Policies (Protect Global Categories)
-- ============================================================================
DROP POLICY IF EXISTS financial_categories_app ON financial_categories;
DROP POLICY IF EXISTS financial_categories_app_select ON financial_categories;
DROP POLICY IF EXISTS financial_categories_app_modify ON financial_categories;

-- Allow reading global categories (NULL) and own property categories
CREATE POLICY financial_categories_app_select ON financial_categories
    FOR SELECT TO pgapp_app
    USING (
        property_id IS NULL
        OR property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
    );

-- Strictly permit INSERT, UPDATE, DELETE only for rows owned by current property
CREATE POLICY financial_categories_app_modify ON financial_categories
    FOR ALL TO pgapp_app
    USING (
        property_id IS NOT NULL
        AND property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
    )
    WITH CHECK (
        property_id IS NOT NULL
        AND property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
    );

-- ============================================================================
-- 5. Parent-Derived and Direct Property Child Tables RLS
-- ============================================================================

-- 5.1 Direct property-scoped operational tables
DO $$
DECLARE
    tbl TEXT;
    direct_tables TEXT[] := ARRAY[
        'floors',
        'rooms',
        'inspections',
        'meter_readings',
        'referrals',
        'notifications'
    ];
BEGIN
    FOREACH tbl IN ARRAY direct_tables LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY;', tbl);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY;', tbl);

        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', tbl || '_app', tbl);
        EXECUTE format($pol$
            CREATE POLICY %I ON %I
                FOR ALL TO pgapp_app
                USING (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
                WITH CHECK (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid);
        $pol$, tbl || '_app', tbl);

        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', tbl || '_maint', tbl);
        EXECUTE format($pol$
            CREATE POLICY %I ON %I
                FOR ALL TO pgapp_maint, pgapp_worker
                USING (true)
                WITH CHECK (true);
        $pol$, tbl || '_maint', tbl);
    END LOOP;
END $$;

-- 5.2 inspection_items (derived via inspections.property_id)
ALTER TABLE inspection_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE inspection_items FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS inspection_items_app ON inspection_items;
CREATE POLICY inspection_items_app ON inspection_items
    FOR ALL TO pgapp_app
    USING (
        EXISTS (
            SELECT 1 FROM inspections i
            WHERE i.id = inspection_items.inspection_id
              AND i.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM inspections i
            WHERE i.id = inspection_items.inspection_id
              AND i.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    );

DROP POLICY IF EXISTS inspection_items_maint ON inspection_items;
CREATE POLICY inspection_items_maint ON inspection_items
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true) WITH CHECK (true);

-- 5.3 due-linked tables (payment_allocations, refund_allocations, payment_in_flight_lock, payment_tokens, payment_intents, payment_intent_dues)
DO $$
DECLARE
    tbl TEXT;
    due_tables TEXT[] := ARRAY[
        'payment_allocations',
        'refund_allocations',
        'payment_in_flight_lock',
        'payment_tokens',
        'payment_intents',
        'payment_intent_dues'
    ];
BEGIN
    FOREACH tbl IN ARRAY due_tables LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY;', tbl);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY;', tbl);

        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', tbl || '_app', tbl);
        EXECUTE format($pol$
            CREATE POLICY %I ON %I
                FOR ALL TO pgapp_app
                USING (
                    EXISTS (
                        SELECT 1 FROM dues d
                        WHERE d.id = %I.due_id
                          AND d.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
                    )
                )
                WITH CHECK (
                    EXISTS (
                        SELECT 1 FROM dues d
                        WHERE d.id = %I.due_id
                          AND d.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
                    )
                );
        $pol$, tbl || '_app', tbl, tbl, tbl);

        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', tbl || '_maint', tbl);
        EXECUTE format($pol$
            CREATE POLICY %I ON %I
                FOR ALL TO pgapp_maint, pgapp_worker
                USING (true) WITH CHECK (true);
        $pol$, tbl || '_maint', tbl);
    END LOOP;
END $$;

-- 5.4 push_subscriptions, points_ledger, tenant_streaks, redemptions (derived via tenants.property_id)
DO $$
DECLARE
    tbl TEXT;
    tenant_tables TEXT[] := ARRAY[
        'push_subscriptions',
        'points_ledger',
        'tenant_streaks',
        'redemptions'
    ];
BEGIN
    FOREACH tbl IN ARRAY tenant_tables LOOP
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY;', tbl);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY;', tbl);

        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', tbl || '_app', tbl);
        EXECUTE format($pol$
            CREATE POLICY %I ON %I
                FOR ALL TO pgapp_app
                USING (
                    EXISTS (
                        SELECT 1 FROM tenants t
                        WHERE t.id = %I.tenant_id
                          AND t.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
                    )
                )
                WITH CHECK (
                    EXISTS (
                        SELECT 1 FROM tenants t
                        WHERE t.id = %I.tenant_id
                          AND t.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
                    )
                );
        $pol$, tbl || '_app', tbl, tbl, tbl);

        EXECUTE format('DROP POLICY IF EXISTS %I ON %I;', tbl || '_maint', tbl);
        EXECUTE format($pol$
            CREATE POLICY %I ON %I
                FOR ALL TO pgapp_maint, pgapp_worker
                USING (true) WITH CHECK (true);
        $pol$, tbl || '_maint', tbl);
    END LOOP;
END $$;

-- 5.5 notification_deliveries (derived via notifications.property_id)
ALTER TABLE notification_deliveries ENABLE ROW LEVEL SECURITY;
ALTER TABLE notification_deliveries FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS notification_deliveries_app ON notification_deliveries;
CREATE POLICY notification_deliveries_app ON notification_deliveries
    FOR ALL TO pgapp_app
    USING (
        EXISTS (
            SELECT 1 FROM notifications n
            WHERE n.id = notification_deliveries.notification_id
              AND n.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    )
    WITH CHECK (
        EXISTS (
            SELECT 1 FROM notifications n
            WHERE n.id = notification_deliveries.notification_id
              AND n.property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid
        )
    );

DROP POLICY IF EXISTS notification_deliveries_maint ON notification_deliveries;
CREATE POLICY notification_deliveries_maint ON notification_deliveries
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true) WITH CHECK (true);

-- 5.6 outbox_events (worker/maint queue - fail closed for application role)
ALTER TABLE outbox_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS outbox_events_app ON outbox_events;
CREATE POLICY outbox_events_app ON outbox_events
    FOR ALL TO pgapp_app
    USING (false) WITH CHECK (false);

DROP POLICY IF EXISTS outbox_events_maint ON outbox_events;
CREATE POLICY outbox_events_maint ON outbox_events
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true) WITH CHECK (true);

-- ============================================================================
-- 6. Role Permissions on Newly Covered Tables
-- ============================================================================
GRANT SELECT, INSERT, UPDATE, DELETE ON
    floors,
    rooms,
    inspections,
    inspection_items,
    meter_readings,
    referrals,
    payment_intents,
    payment_allocations,
    refund_allocations,
    payment_intent_dues,
    payment_in_flight_lock,
    payment_tokens,
    push_subscriptions,
    points_ledger,
    tenant_streaks,
    redemptions,
    notifications,
    notification_deliveries
TO pgapp_app, pgapp_maint, pgapp_worker;

GRANT SELECT, INSERT, UPDATE, DELETE ON outbox_events TO pgapp_maint, pgapp_worker;
