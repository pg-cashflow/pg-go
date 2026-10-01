-- Migration 029: Per-Property Operational Settings Table
-- Controls operational feature flags: payout auto-dispatch, reminder offsets, active modules

CREATE TABLE IF NOT EXISTS property_settings (
    property_id UUID PRIMARY KEY REFERENCES properties(id) ON DELETE CASCADE,
    payout_auto_dispatch BOOLEAN NOT NULL DEFAULT FALSE,
    reminder_offsets INTEGER[] NOT NULL DEFAULT '{-3, 0, 1, 7}',
    reminder_catch_up_days INTEGER NOT NULL DEFAULT 2,
    active_modules JSONB NOT NULL DEFAULT '{"gamification": true, "kyc": true, "payouts": true, "accounting": true, "calendar": true}'::jsonb,
    auto_apply_credit BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Backfill default settings for any existing properties
INSERT INTO property_settings (property_id)
SELECT id FROM properties
ON CONFLICT (property_id) DO NOTHING;
