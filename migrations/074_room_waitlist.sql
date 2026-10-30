SET lock_timeout = '5s';

-- Migration 074: Room waitlist (P1) + bound slot invites for offers.

ALTER TABLE property_onboarding_settings
  ADD COLUMN IF NOT EXISTS waitlist_offer_ttl_hours INT NOT NULL DEFAULT 48;

ALTER TABLE slot_invites
  ADD COLUMN IF NOT EXISTS bound_phone VARCHAR(16),
  ADD COLUMN IF NOT EXISTS waitlist_entry_id UUID;

CREATE TABLE IF NOT EXISTS room_waitlist_entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    room_id UUID NOT NULL REFERENCES rooms(id) ON DELETE CASCADE,
    user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    phone VARCHAR(16) NOT NULL,
    status TEXT NOT NULL DEFAULT 'waiting',
    offered_slot_invite_id UUID REFERENCES slot_invites(id) ON DELETE SET NULL,
    offer_expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_room_waitlist_status CHECK (status IN ('waiting', 'offered', 'accepted', 'cancelled'))
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_room_waitlist_room_phone_active
    ON room_waitlist_entries(room_id, phone)
    WHERE status IN ('waiting', 'offered');

CREATE INDEX IF NOT EXISTS idx_room_waitlist_room_waiting
    ON room_waitlist_entries(room_id, created_at)
    WHERE status = 'waiting';

CREATE INDEX IF NOT EXISTS idx_room_waitlist_offer_expiry
    ON room_waitlist_entries(offer_expires_at)
    WHERE status = 'offered' AND offer_expires_at IS NOT NULL;

ALTER TABLE slot_invites
  ADD CONSTRAINT fk_slot_invites_waitlist_entry
  FOREIGN KEY (waitlist_entry_id) REFERENCES room_waitlist_entries(id) ON DELETE SET NULL;

ALTER TABLE room_waitlist_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE room_waitlist_entries FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS room_waitlist_entries_app ON room_waitlist_entries;
CREATE POLICY room_waitlist_entries_app ON room_waitlist_entries
    FOR ALL TO pgapp_app
    USING (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid)
    WITH CHECK (property_id = NULLIF(current_setting('app.current_property_id', true), '')::uuid);

DROP POLICY IF EXISTS room_waitlist_entries_maint ON room_waitlist_entries;
CREATE POLICY room_waitlist_entries_maint ON room_waitlist_entries
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true) WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE ON room_waitlist_entries TO pgapp_app, pgapp_maint, pgapp_worker;
