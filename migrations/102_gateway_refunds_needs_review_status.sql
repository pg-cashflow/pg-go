SET lock_timeout = '5s';

-- Migration 102: Add needs_review status to gateway_refunds
-- Standard: ASD-STE100
-- Reference: Distinguish refund not found at gateway from merchant balance on_hold

DO $$
DECLARE
    chk_name TEXT;
BEGIN
    FOR chk_name IN
        SELECT conname
        FROM pg_constraint
        WHERE conrelid = 'gateway_refunds'::regclass
          AND contype = 'c'
          AND pg_get_constraintdef(oid) LIKE '%status%initiated%'
    LOOP
        EXECUTE 'ALTER TABLE gateway_refunds DROP CONSTRAINT ' || quote_ident(chk_name);
    END LOOP;
END $$;

ALTER TABLE gateway_refunds
    ADD CONSTRAINT chk_gateway_refunds_status
    CHECK (status IN ('initiated', 'pending', 'on_hold', 'needs_review', 'succeeded', 'failed', 'cancelled'));
