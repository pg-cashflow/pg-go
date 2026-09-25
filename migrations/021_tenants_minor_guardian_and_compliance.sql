-- Migration 021: Minor Guardian Protection, 1-31 Anchor Clamping & FSSAI Gate

-- 1. Extend due_day to 1-31 to support month-end anchor days
ALTER TABLE tenants DROP CONSTRAINT IF EXISTS tenants_due_day_check;
ALTER TABLE tenants DROP CONSTRAINT IF EXISTS chk_tenants_due_day;
ALTER TABLE tenants ADD CONSTRAINT chk_tenants_due_day CHECK (due_day BETWEEN 1 AND 31);

-- 2. Minor Tenant & DigiLocker Guardian Verification Fields
ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS majority_date DATE,
    ADD COLUMN IF NOT EXISTS guardian_name VARCHAR(100),
    ADD COLUMN IF NOT EXISTS guardian_phone VARCHAR(15),
    ADD COLUMN IF NOT EXISTS guardian_relation VARCHAR(30),
    ADD COLUMN IF NOT EXISTS guardian_kyc_reference_id TEXT,
    ADD COLUMN IF NOT EXISTS guardian_consent_verified_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS is_gamification_disabled BOOLEAN NOT NULL DEFAULT FALSE;

-- 3. Dynamic Late-Payment Settings & Grace Days on Properties
ALTER TABLE property_gamification_settings
    ADD COLUMN IF NOT EXISTS grace_days INT NOT NULL DEFAULT 2,
    ADD COLUMN IF NOT EXISTS late_penalty_points_per_day INT NOT NULL DEFAULT 2,
    ADD COLUMN IF NOT EXISTS late_penalty_max_points INT NOT NULL DEFAULT 50;

-- 4. Milestone Farming & Streak Replay Tracking
CREATE TABLE IF NOT EXISTS tenant_milestone_awards (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    milestone_months INT NOT NULL,
    awarded_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_tenant_milestone UNIQUE (tenant_id, milestone_months)
);

CREATE TABLE IF NOT EXISTS tenant_streak_due_events (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    due_id UUID NOT NULL REFERENCES dues(id) ON DELETE CASCADE,
    processed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_tenant_streak_due UNIQUE (tenant_id, due_id)
);

-- 5. Physical Hostel Mess Compliance & Premises Document Gate
ALTER TABLE properties
    ADD COLUMN IF NOT EXISTS fssai_registration_number TEXT,
    ADD COLUMN IF NOT EXISTS mess_operational_status TEXT NOT NULL DEFAULT 'unlicensed' CHECK (mess_operational_status IN ('unlicensed', 'licensed', 'exempt')),
    ADD COLUMN IF NOT EXISTS lease_or_noc_document_id UUID;

-- Decoupled Gate: payment_mode = 'cashfree' requires registered premises documentation (lease/NOC),
-- while FSSAI is tracked strictly for physical mess/kitchen operational compliance.
ALTER TABLE properties DROP CONSTRAINT IF EXISTS chk_property_cashfree_compliance;
ALTER TABLE properties ADD CONSTRAINT chk_property_cashfree_compliance
    CHECK (payment_mode <> 'cashfree' OR lease_or_noc_document_id IS NOT NULL) NOT VALID;
