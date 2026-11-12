SET lock_timeout = '5s';

-- Migration 109: Allow manager as gateway_refunds source and valid initiator
-- Standard: ASD-STE100
-- Reference: F-01/F-11 audit defect fix for manager refund attribution

DO $$
DECLARE
    chk_name TEXT;
BEGIN
    FOR chk_name IN
        SELECT conname
        FROM pg_constraint
        WHERE conrelid = 'gateway_refunds'::regclass
          AND contype = 'c'
          AND (pg_get_constraintdef(oid) LIKE '%source%owner%' OR conname = 'chk_refund_initiator')
    LOOP
        EXECUTE 'ALTER TABLE gateway_refunds DROP CONSTRAINT ' || quote_ident(chk_name);
    END LOOP;
END $$;

ALTER TABLE gateway_refunds
    ADD CONSTRAINT chk_gateway_refunds_source
    CHECK (source IN ('owner', 'manager', 'system', 'cashfree_auto', 'dashboard'));

ALTER TABLE gateway_refunds
    ADD CONSTRAINT chk_refund_initiator
    CHECK (
        (source IN ('owner', 'manager') AND initiated_by IS NOT NULL) OR
        (source NOT IN ('owner', 'manager') AND initiated_by IS NULL)
    );
