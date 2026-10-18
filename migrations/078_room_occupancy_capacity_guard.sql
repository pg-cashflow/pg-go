-- Migration 078: Defense-in-depth guard against overbooking a room.
--
-- The application-level fix (FOR UPDATE on rooms during slot-invite creation,
-- see internal/postgres/slot_invite_repo.go roomSnapshot + internal/slotinvite/
-- service.go Create) is the primary fix for the invite-creation race condition.
-- This trigger is a second, independent line of defense: if any future code
-- path updates room_occupancy without going through the locked path above
-- (a bulk-import script, a manual SQL fix, a bug), the database itself refuses
-- to let reserved + active-tenant beds exceed the room's physical capacity.
--
-- allow_over_capacity rooms are intentionally exempt up to max_extra_beds_per_room
-- (owner-approved overbooking, e.g. temporary cot/floor bed) — that ceiling is
-- enforced in application code (CreateOpen's MaxExtraBeds check) since it is a
-- per-create decision, not a structural invariant; this trigger only guards the
-- structural invariant (never sell a bed that doesn't exist and isn't an
-- explicitly approved extra).

CREATE OR REPLACE FUNCTION fn_room_occupancy_capacity_guard()
RETURNS TRIGGER AS $$
DECLARE
    room_capacity INT;
    active_tenant_beds INT;
    total_claimed INT;
BEGIN
    -- Row is already locked by the caller's transaction (rooms FOR UPDATE in
    -- roomSnapshot) in the normal path; SELECT here re-reads within that same
    -- lock scope. For the out-of-band paths this trigger exists to catch,
    -- this SELECT still reads the committed capacity value correctly.
    SELECT capacity INTO room_capacity FROM rooms WHERE id = NEW.room_id;
    IF room_capacity IS NULL THEN
        RAISE EXCEPTION 'room_occupancy_capacity_guard: room % not found', NEW.room_id;
    END IF;

    SELECT COUNT(*) INTO active_tenant_beds
    FROM tenants
    WHERE room_id = NEW.room_id AND status IN ('active', 'pending_allocation');

    total_claimed := active_tenant_beds + NEW.open_invite_reserved;

    IF total_claimed > room_capacity + NEW.over_capacity_allocated THEN
        RAISE EXCEPTION
            'room_occupancy_capacity_guard: room % would exceed capacity (tenants=%, reserved=%, capacity=%, over_capacity_allocated=%)',
            NEW.room_id, active_tenant_beds, NEW.open_invite_reserved, room_capacity, NEW.over_capacity_allocated
            USING ERRCODE = '23514'; -- check_violation, so callers can pattern-match on SQLSTATE
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_room_occupancy_capacity_guard ON room_occupancy;
CREATE TRIGGER trg_room_occupancy_capacity_guard
    BEFORE INSERT OR UPDATE OF open_invite_reserved, over_capacity_allocated ON room_occupancy
    FOR EACH ROW
    EXECUTE FUNCTION fn_room_occupancy_capacity_guard();
