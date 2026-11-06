SET lock_timeout = '5s';

-- Migration 097: Ensure sum of refund allocations matches refund amount.
-- Standard: ASD-STE100
-- Method: Empirical First-Principles Verification
-- Reference: Finding P1 (refund allocations must completely absorb applied refund amount)
--
-- This deferred constraint trigger enforces that for any applied payment
-- (payments.is_unapplied IS FALSE), an active gateway refund must have
-- refund_allocations whose sum of amount_paise exactly equals the refund's amount_paise.

CREATE OR REPLACE FUNCTION fn_refund_allocation_sum_guard()
RETURNS TRIGGER AS $$
DECLARE
    v_is_unapplied BOOLEAN;
    v_total_alloc BIGINT;
BEGIN
    -- Unapplied payments do not map to dues
    SELECT is_unapplied INTO v_is_unapplied
    FROM payments
    WHERE id = NEW.payment_id;

    IF v_is_unapplied IS TRUE THEN
        RETURN NEW;
    END IF;

    -- Disregard terminal non-settling states
    IF NEW.status IN ('failed', 'cancelled') THEN
        RETURN NEW;
    END IF;

    -- Compute total allocations committed for this refund
    SELECT COALESCE(SUM(amount_paise), 0)
    INTO v_total_alloc
    FROM refund_allocations
    WHERE refund_id = NEW.id;

    IF v_total_alloc <> NEW.amount_paise THEN
        RAISE EXCEPTION
            'refund_allocation_sum_guard: allocations sum (% paise) does not equal refund amount (% paise) for refund %',
            v_total_alloc, NEW.amount_paise, NEW.id
            USING ERRCODE = '23514'; -- check_violation
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_refund_allocation_sum_guard ON gateway_refunds;
CREATE CONSTRAINT TRIGGER trg_refund_allocation_sum_guard
    AFTER INSERT OR UPDATE OF amount_paise, status ON gateway_refunds
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW
    EXECUTE FUNCTION fn_refund_allocation_sum_guard();
