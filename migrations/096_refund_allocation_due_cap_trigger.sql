SET lock_timeout = '5s';

-- Migration 096: Guard against over-refunding a due across concurrent or sequential refunds.
-- Standard: ASD-STE100
-- Method: Empirical First-Principles Verification
-- Reference: Finding 1 (double-assignment of refunds to dues)
--
-- This trigger enforces the invariant that active refund allocations
-- (status NOT IN ('failed', 'cancelled')) for any due must not exceed
-- the total payments allocated to that due minus any departure adjustments.

CREATE OR REPLACE FUNCTION fn_refund_allocation_due_cap_guard()
RETURNS TRIGGER AS $$
DECLARE
    v_total_paid BIGINT;
    v_departure_adjustments BIGINT;
    v_other_refunded BIGINT;
    v_max_refundable BIGINT;
BEGIN
    IF NEW.due_id IS NULL THEN
        RETURN NEW;
    END IF;

    -- Calculate total payment allocations to this due
    SELECT COALESCE(SUM(amount_paise), 0)
    INTO v_total_paid
    FROM payment_allocations
    WHERE due_id = NEW.due_id;

    -- Calculate departure due adjustments if any
    SELECT COALESCE(SUM(amount_paise), 0)
    INTO v_departure_adjustments
    FROM departure_due_adjustments
    WHERE due_id = NEW.due_id;

    -- Calculate all other active refund allocations for this due
    SELECT COALESCE(SUM(ra.amount_paise), 0)
    INTO v_other_refunded
    FROM refund_allocations ra
    JOIN gateway_refunds gr ON gr.id = ra.refund_id
    WHERE ra.due_id = NEW.due_id
      AND gr.status NOT IN ('failed', 'cancelled')
      AND ra.id IS DISTINCT FROM NEW.id;

    v_max_refundable := v_total_paid - v_departure_adjustments - v_other_refunded;
    IF v_max_refundable < 0 THEN
        v_max_refundable := 0;
    END IF;

    IF NEW.amount_paise > v_max_refundable THEN
        RAISE EXCEPTION
            'refund_allocation_due_cap_guard: refund allocation of % paise for due % exceeds remaining refundable limit of % paise (total_paid=%, adjustments=%, other_refunded=%)',
            NEW.amount_paise, NEW.due_id, v_max_refundable, v_total_paid, v_departure_adjustments, v_other_refunded
            USING ERRCODE = '23514'; -- check_violation
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_refund_allocation_due_cap_guard ON refund_allocations;
CREATE TRIGGER trg_refund_allocation_due_cap_guard
    BEFORE INSERT OR UPDATE OF due_id, amount_paise ON refund_allocations
    FOR EACH ROW
    EXECUTE FUNCTION fn_refund_allocation_due_cap_guard();

