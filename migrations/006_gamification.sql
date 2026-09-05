-- Migration 006: PG Gamification (Clean + Timely Pay, Food RSVP, Water & Electricity Metering)

-- 1. Floors & Rooms
CREATE TABLE IF NOT EXISTS floors (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id  UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  floor_number INT NOT NULL,
  name         VARCHAR(50) NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_floors_property_floor_number UNIQUE (property_id, floor_number)
);
CREATE INDEX IF NOT EXISTS idx_floors_property ON floors(property_id);

CREATE TABLE IF NOT EXISTS rooms (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id    UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  floor_id       UUID NOT NULL REFERENCES floors(id) ON DELETE CASCADE,
  room_number    VARCHAR(20) NOT NULL,
  capacity       SMALLINT NOT NULL DEFAULT 2,
  included_units INT NOT NULL DEFAULT 50, -- monthly kWh included in base rent
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_rooms_property_room_number UNIQUE (property_id, room_number)
);
CREATE INDEX IF NOT EXISTS idx_rooms_property ON rooms(property_id);
CREATE INDEX IF NOT EXISTS idx_rooms_floor ON rooms(floor_id);

-- Link tenants to room_id while keeping room_number for backward compat
ALTER TABLE tenants ADD COLUMN IF NOT EXISTS room_id UUID REFERENCES rooms(id);
CREATE INDEX IF NOT EXISTS idx_tenants_room_id ON tenants(room_id);

-- Backfill default Ground Floor (floor 0) and rooms for any existing tenants with room_number
DO $$
DECLARE
  prop RECORD;
  fl_id UUID;
  t RECORD;
  rm_id UUID;
BEGIN
  FOR prop IN SELECT DISTINCT property_id FROM tenants WHERE room_number IS NOT NULL AND room_number != '' LOOP
    SELECT id INTO fl_id FROM floors WHERE property_id = prop.property_id AND floor_number = 0;
    IF fl_id IS NULL THEN
      INSERT INTO floors (property_id, floor_number, name)
      VALUES (prop.property_id, 0, 'Ground Floor')
      RETURNING id INTO fl_id;
    END IF;

    FOR t IN SELECT DISTINCT room_number FROM tenants WHERE property_id = prop.property_id AND room_number IS NOT NULL AND room_number != '' LOOP
      SELECT id INTO rm_id FROM rooms WHERE property_id = prop.property_id AND room_number = t.room_number;
      IF rm_id IS NULL THEN
        INSERT INTO rooms (property_id, floor_id, room_number)
        VALUES (prop.property_id, fl_id, t.room_number)
        RETURNING id INTO rm_id;
      END IF;

      UPDATE tenants
      SET room_id = rm_id
      WHERE property_id = prop.property_id AND room_number = t.room_number AND room_id IS NULL;
    END LOOP;
  END LOOP;
END $$;

-- 2. Property Gamification Settings
CREATE TABLE IF NOT EXISTS property_gamification_settings (
  property_id           UUID PRIMARY KEY REFERENCES properties(id) ON DELETE CASCADE,
  point_value_paise     INT NOT NULL DEFAULT 100,      -- 1 point = Re 1 (100 paise)
  monthly_budget_paise  INT NOT NULL DEFAULT 1000000,  -- max points budget per month in paise (Rs 10,000)
  earn_cap_per_tenant   INT NOT NULL DEFAULT 200,      -- general monthly earn cap (points)
  rsvp_sub_cap          INT NOT NULL DEFAULT 60,       -- isolated sub-cap for food RSVP points
  expiry_days           INT NOT NULL DEFAULT 180,      -- points expire in 6 months
  floor_bonus_threshold INT NOT NULL DEFAULT 85,       -- 85% floor cleanliness average unlocks floor bonus
  electricity_tariff_paise INT NOT NULL DEFAULT 1000,  -- Rs 10 per excess unit
  created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Seed default settings for existing properties
INSERT INTO property_gamification_settings (property_id)
SELECT id FROM properties
ON CONFLICT (property_id) DO NOTHING;

-- 3. Point Rules
CREATE TABLE IF NOT EXISTS point_rules (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id  UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  code         VARCHAR(50) NOT NULL,
  name         VARCHAR(100) NOT NULL,
  description  TEXT NOT NULL DEFAULT '',
  points       INT NOT NULL,
  monthly_cap  INT NOT NULL DEFAULT 0, -- 0 = no rule-level cap
  is_rsvp      BOOLEAN NOT NULL DEFAULT FALSE,
  active       BOOLEAN NOT NULL DEFAULT TRUE,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_point_rules_property_code UNIQUE (property_id, code)
);
CREATE INDEX IF NOT EXISTS idx_point_rules_property ON point_rules(property_id);

-- Seed default point rules for all properties
INSERT INTO point_rules (property_id, code, name, description, points, monthly_cap, is_rsvp)
SELECT p.id, r.code, r.name, r.description, r.points, r.monthly_cap, r.is_rsvp
FROM properties p
CROSS JOIN (
  VALUES
    ('RENT_ON_TIME', 'On-time Rent Payment', 'Paid rent on or before due date', 50, 50, FALSE),
    ('ROOM_CLEAN', 'Weekly Room Clean', 'Passed weekly room inspection', 15, 60, FALSE),
    ('FLOOR_CLEAN_BONUS', 'Floor Cleanliness Multiplier', 'Floor achieved >=85% clean score bonus', 30, 30, FALSE),
    ('FLOOR_CAPTAIN_DUTY', 'Floor Captain Duty', 'Managed common area 5S checklist for the week', 20, 40, FALSE),
    ('NO_INCIDENT_MONTH', 'Zero Incident Month', 'Floor maintained 0 safety incidents this month', 20, 20, FALSE),
    ('MEAL_RSVP_ON_TIME', 'Meal RSVP On-Time', 'Confirmed next day meals before 8pm cutoff', 2, 60, TRUE),
    ('HAZARD_REPORT', 'Hazard / Maintenance Report', 'Reported verified safety hazard or water leak', 25, 100, FALSE),
    ('ENERGY_SAVER_MONTH', 'Energy Saver of Month', 'Lowest consumption room on floor', 50, 50, FALSE),
    ('REFERRAL_JOIN', 'Referral Move-in', 'Referred friend moved in and paid first rent', 500, 1000, FALSE)
) AS r(code, name, description, points, monthly_cap, is_rsvp)
ON CONFLICT (property_id, code) DO NOTHING;

-- 4. Points Ledger (Append-only)
-- Invariant: negative deltas (spent or deducted) ALWAYS have expires_at = NULL so they never expire back into balance.
CREATE TABLE IF NOT EXISTS points_ledger (
  id           BIGSERIAL PRIMARY KEY,
  tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  property_id  UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  rule_code    VARCHAR(50) NOT NULL,
  delta        INT NOT NULL,
  ref_type     VARCHAR(50), -- due, inspection, violation, rsvp, hazard, referral
  ref_id       VARCHAR(100),
  expires_at   TIMESTAMPTZ, -- NULL for negative deltas
  created_by   UUID REFERENCES users(id),
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_points_ledger_tenant_balance ON points_ledger(tenant_id, delta, expires_at);
CREATE INDEX IF NOT EXISTS idx_points_ledger_property_month ON points_ledger(property_id, created_at);
CREATE UNIQUE INDEX IF NOT EXISTS uq_points_ledger_idempotency ON points_ledger(tenant_id, rule_code, ref_type, ref_id)
  WHERE ref_type IS NOT NULL AND ref_id IS NOT NULL;

-- 5. Tenant Streaks & Cached Balance
CREATE TABLE IF NOT EXISTS tenant_streaks (
  tenant_id            UUID PRIMARY KEY REFERENCES tenants(id) ON DELETE CASCADE,
  property_id          UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  on_time_months       INT NOT NULL DEFAULT 0,
  cached_balance       INT NOT NULL DEFAULT 0,
  last_on_time_due_id  UUID REFERENCES dues(id),
  freezes_available    SMALLINT NOT NULL DEFAULT 1, -- 1 freeze every 6 months
  last_freeze_used_at  TIMESTAMPTZ,
  updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_tenant_streaks_property ON tenant_streaks(property_id);

-- Initialize streaks for existing active tenants
INSERT INTO tenant_streaks (tenant_id, property_id)
SELECT id, property_id FROM tenants
ON CONFLICT (tenant_id) DO NOTHING;

-- 6. Rewards Catalog & Redemptions
CREATE TABLE IF NOT EXISTS rewards_catalog (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id       UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  code              VARCHAR(50) NOT NULL,
  title             VARCHAR(100) NOT NULL,
  description       TEXT NOT NULL DEFAULT '',
  category          VARCHAR(30) NOT NULL, -- cash_credit, food_coupon, perk
  points_cost       INT NOT NULL,
  min_tenure_months INT NOT NULL DEFAULT 0, -- cash credit requires 3
  is_active         BOOLEAN NOT NULL DEFAULT TRUE,
  metadata          JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_rewards_catalog_property_code UNIQUE (property_id, code)
);
CREATE INDEX IF NOT EXISTS idx_rewards_catalog_property ON rewards_catalog(property_id);

-- Seed default rewards catalog
INSERT INTO rewards_catalog (property_id, code, title, description, category, points_cost, min_tenure_months, metadata)
SELECT p.id, r.code, r.title, r.description, r.category, r.points_cost, r.min_tenure_months, r.metadata::jsonb
FROM properties p
CROSS JOIN (
  VALUES
    ('RENT_CREDIT_500', 'Rs 500 Rent Credit', 'Deducted directly from next month rent. Requires 3 months consecutive on-time rent.', 'cash_credit', 500, 3, '{"discount_paise": 50000}'),
    ('FOOD_COUPON_200', 'Rs 200 Mess / Tiffin Coupon', 'Digital voucher for local partner mess or tiffin delivery.', 'food_coupon', 200, 0, '{"discount_paise": 20000}'),
    ('GUEST_PASS', 'Guest Stay Pass', '1 night guest stay without surcharge (subject to warden notice).', 'perk', 100, 0, '{}'),
    ('DEPOSIT_FAST_TRACK', '48h Deposit Refund Fast-Track', 'Guaranteed deposit return within 48h of move-out inspection.', 'perk', 150, 0, '{}'),
    ('RENT_HIKE_FREEZE', '6-Month Rent Freeze', 'Locks current rent amount for 6 months beyond agreement renewal.', 'perk', 400, 0, '{}'),
    ('LATE_FEE_WAIVER', 'Late Fee Waiver Token', 'One-time waiver token for late payment fee.', 'perk', 100, 0, '{}')
) AS r(code, title, description, category, points_cost, min_tenure_months, metadata)
ON CONFLICT (property_id, code) DO NOTHING;

CREATE TABLE IF NOT EXISTS redemptions (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  property_id     UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  reward_id       UUID NOT NULL REFERENCES rewards_catalog(id),
  points_spent    INT NOT NULL,
  status          VARCHAR(20) NOT NULL DEFAULT 'completed', -- completed, cancelled
  applied_due_id  UUID REFERENCES dues(id),
  coupon_code     VARCHAR(50),
  metadata        JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_redemptions_tenant ON redemptions(tenant_id);
CREATE INDEX IF NOT EXISTS idx_redemptions_property ON redemptions(property_id);

-- 7. Inspections & Inspection Items
CREATE TABLE IF NOT EXISTS inspections (
  id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id         UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  room_id             UUID REFERENCES rooms(id) ON DELETE CASCADE,
  floor_id            UUID REFERENCES floors(id) ON DELETE CASCADE,
  inspector_user_id   UUID NOT NULL REFERENCES users(id),
  inspection_type     VARCHAR(20) NOT NULL, -- room, floor, common_bathroom
  score_percent       INT NOT NULL,
  passed              BOOLEAN NOT NULL,
  notes               TEXT NOT NULL DEFAULT '',
  inspected_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_inspections_property_date ON inspections(property_id, inspected_at);
CREATE INDEX IF NOT EXISTS idx_inspections_room ON inspections(room_id);
CREATE INDEX IF NOT EXISTS idx_inspections_floor ON inspections(floor_id);

CREATE TABLE IF NOT EXISTS inspection_items (
  id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  inspection_id      UUID NOT NULL REFERENCES inspections(id) ON DELETE CASCADE,
  item_key           VARCHAR(50) NOT NULL,
  description        VARCHAR(150) NOT NULL,
  passed             BOOLEAN NOT NULL,
  photo_bytes        BYTEA, -- mandatory when passed = false
  notes              TEXT NOT NULL DEFAULT '',
  disputed_at        TIMESTAMPTZ,
  dispute_note       TEXT,
  resolved_at        TIMESTAMPTZ,
  resolved_by        UUID REFERENCES users(id),
  resolution_status  VARCHAR(20) NOT NULL DEFAULT 'none' -- none, disputed, upheld, overturned
);
CREATE INDEX IF NOT EXISTS idx_inspection_items_inspection ON inspection_items(inspection_id);

-- Kitchen Vendor Accountability Audits (Separate from tenant points)
CREATE TABLE IF NOT EXISTS vendor_inspections (
  id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id        UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  inspector_user_id  UUID NOT NULL REFERENCES users(id),
  vendor_name        VARCHAR(100) NOT NULL,
  inspection_type    VARCHAR(30) NOT NULL DEFAULT 'kitchen',
  score_percent      INT NOT NULL,
  notes              TEXT NOT NULL DEFAULT '',
  photo_bytes        BYTEA,
  penalty_paise      INT NOT NULL DEFAULT 0,
  inspected_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_vendor_inspections_property ON vendor_inspections(property_id);

-- 8. Violations & Anonymous Hazards
CREATE TABLE IF NOT EXISTS violations (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  property_id     UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  rule_code       VARCHAR(50) NOT NULL,
  severity        VARCHAR(20) NOT NULL CHECK (severity IN ('safety', 'lifestyle')),
  step            SMALLINT NOT NULL DEFAULT 1, -- 1: notice, 2: deduction, 3: warning/disqualification
  description     TEXT NOT NULL,
  evidence_bytes  BYTEA,
  acknowledged_at TIMESTAMPTZ,
  created_by      UUID NOT NULL REFERENCES users(id),
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_violations_tenant_90d ON violations(tenant_id, created_at);
CREATE INDEX IF NOT EXISTS idx_violations_property ON violations(property_id);

CREATE TABLE IF NOT EXISTS hazards (
  id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id            UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  reported_by_tenant_id  UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  category               VARCHAR(50) NOT NULL, -- gas_smell, electrical_wire, water_leak, ro_purifier, other
  description            TEXT NOT NULL,
  photo_bytes            BYTEA,
  status                 VARCHAR(20) NOT NULL DEFAULT 'open', -- open, in_progress, resolved, rejected
  resolved_at            TIMESTAMPTZ,
  resolved_by            UUID REFERENCES users(id),
  points_awarded         BOOLEAN NOT NULL DEFAULT FALSE,
  created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_hazards_property_status ON hazards(property_id, status);

-- 9. Meter Readings & Utilities
CREATE TABLE IF NOT EXISTS meter_readings (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id    UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  room_id        UUID REFERENCES rooms(id) ON DELETE CASCADE,
  floor_id       UUID REFERENCES floors(id) ON DELETE CASCADE,
  kind           VARCHAR(20) NOT NULL CHECK (kind IN ('electricity', 'water')),
  reading_value  NUMERIC(10, 2) NOT NULL,
  reading_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  source         VARCHAR(20) NOT NULL DEFAULT 'manual', -- manual, device
  recorded_by    UUID NOT NULL REFERENCES users(id),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_meter_readings_room ON meter_readings(room_id, reading_at);
CREATE INDEX IF NOT EXISTS idx_meter_readings_floor ON meter_readings(floor_id, reading_at);

-- Update dues kind check to include electricity and water
ALTER TABLE dues DROP CONSTRAINT IF EXISTS chk_dues_kind;
ALTER TABLE dues DROP CONSTRAINT IF EXISTS dues_kind_check;
ALTER TABLE dues ADD CONSTRAINT chk_dues_kind CHECK (kind IN ('rent', 'deposit', 'electricity', 'water'));

-- 10. Food: Meal RSVPs & Menu Polls
CREATE TABLE IF NOT EXISTS meal_rsvps (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  property_id  UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  meal_date    DATE NOT NULL,
  meal_slot    VARCHAR(20) NOT NULL CHECK (meal_slot IN ('breakfast', 'lunch', 'dinner')),
  attending    BOOLEAN NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_meal_rsvps_tenant_date_slot UNIQUE (tenant_id, meal_date, meal_slot)
);
CREATE INDEX IF NOT EXISTS idx_meal_rsvps_prop_date ON meal_rsvps(property_id, meal_date, meal_slot);

CREATE TABLE IF NOT EXISTS menu_polls (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id  UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  month_year   VARCHAR(7) NOT NULL, -- e.g. 2026-10
  title        VARCHAR(100) NOT NULL,
  options      JSONB NOT NULL DEFAULT '[]'::jsonb, -- array of { id, name, description }
  closed_at    TIMESTAMPTZ,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_menu_polls_property ON menu_polls(property_id);

CREATE TABLE IF NOT EXISTS menu_votes (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  poll_id      UUID NOT NULL REFERENCES menu_polls(id) ON DELETE CASCADE,
  tenant_id    UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  option_id    VARCHAR(50) NOT NULL,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_menu_votes_poll_tenant UNIQUE (poll_id, tenant_id)
);

-- 11. Referrals
CREATE TABLE IF NOT EXISTS referrals (
  id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id         UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  referrer_tenant_id  UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  referred_tenant_id  UUID REFERENCES tenants(id) ON DELETE SET NULL,
  phone               VARCHAR(15) NOT NULL,
  name                VARCHAR(100) NOT NULL DEFAULT '',
  status              VARCHAR(20) NOT NULL DEFAULT 'pending', -- pending, moved_in, rewarded
  points_awarded      INT NOT NULL DEFAULT 0,
  created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  rewarded_at         TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_referrals_referrer ON referrals(referrer_tenant_id);
CREATE INDEX IF NOT EXISTS idx_referrals_property ON referrals(property_id);
