SET lock_timeout = '5s';

-- Migration 073: First-rent referral (owner bps, bind at join, credit ledger).

ALTER TABLE property_onboarding_settings
  ADD COLUMN IF NOT EXISTS referral_enabled BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS joiner_discount_bps INT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS referrer_reward_bps INT NOT NULL DEFAULT 0;

ALTER TABLE referrals ALTER COLUMN referrer_tenant_id DROP NOT NULL;

ALTER TABLE referrals
  ADD COLUMN IF NOT EXISTS referral_code VARCHAR(16),
  ADD COLUMN IF NOT EXISTS joiner_discount_bps INT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS referrer_reward_bps INT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS reward_destination VARCHAR(32),
  ADD COLUMN IF NOT EXISTS destination_set_by VARCHAR(32),
  ADD COLUMN IF NOT EXISTS destination_set_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS referrer_external_phone VARCHAR(16),
  ADD COLUMN IF NOT EXISTS referrer_external_name VARCHAR(100) NOT NULL DEFAULT '',
  ADD COLUMN IF NOT EXISTS bound_at TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS first_rent_due_id UUID REFERENCES dues(id),
  ADD COLUMN IF NOT EXISTS rewarded_payment_id UUID REFERENCES payments(id);

CREATE UNIQUE INDEX IF NOT EXISTS idx_referrals_code ON referrals(referral_code) WHERE referral_code IS NOT NULL;

ALTER TABLE referrals DROP CONSTRAINT IF EXISTS chk_referrals_reward_destination;
ALTER TABLE referrals ADD CONSTRAINT chk_referrals_reward_destination CHECK (
  reward_destination IS NULL OR reward_destination IN ('referrer_credit', 'pass_to_joiner')
);

CREATE TABLE IF NOT EXISTS referral_credit_entries (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    tenant_id UUID REFERENCES tenants(id) ON DELETE SET NULL,
    external_phone VARCHAR(16),
    referral_id UUID NOT NULL REFERENCES referrals(id) ON DELETE CASCADE,
    entry_type VARCHAR(32) NOT NULL,
    amount_paise BIGINT NOT NULL,
    idempotency_key TEXT NOT NULL,
    due_id UUID REFERENCES dues(id),
    payment_id UUID REFERENCES payments(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_referral_credit_idem UNIQUE (idempotency_key),
    CONSTRAINT chk_referral_credit_type CHECK (entry_type IN ('earn', 'spend_rent', 'reversal', 'payout'))
);

CREATE TABLE IF NOT EXISTS due_referral_adjustments (
    due_id UUID PRIMARY KEY REFERENCES dues(id) ON DELETE CASCADE,
    referral_id UUID NOT NULL REFERENCES referrals(id),
    joiner_discount_paise BIGINT NOT NULL DEFAULT 0,
    referrer_pass_paise BIGINT NOT NULL DEFAULT 0,
    rent_base_paise BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_referral_credit_tenant ON referral_credit_entries(property_id, tenant_id) WHERE tenant_id IS NOT NULL;
