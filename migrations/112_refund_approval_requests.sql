SET lock_timeout = '5s';

-- Migration 112: Refund Approval Requests and Dual Control Maker-Checker
-- Standard: ASD-STE100
-- Purpose: Enforce maker-checker gating, immutable transaction details binding,
-- and dual distinct owner approvals for high-value refunds.

CREATE TABLE IF NOT EXISTS refund_approval_requests (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    payment_id UUID NOT NULL REFERENCES payments(id) ON DELETE CASCADE,
    tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    amount_paise BIGINT NOT NULL CHECK (amount_paise > 0),
    reason TEXT NOT NULL,
    idempotency_key VARCHAR(128) NOT NULL,
    requested_by UUID NOT NULL REFERENCES users(id),
    requested_by_role VARCHAR(32) NOT NULL,
    status VARCHAR(32) NOT NULL DEFAULT 'pending',
    required_approvals INT NOT NULL DEFAULT 2,
    material_hash TEXT NOT NULL,
    approvals JSONB NOT NULL DEFAULT '[]'::jsonb,
    rejected_by UUID REFERENCES users(id),
    rejected_at TIMESTAMPTZ,
    rejection_reason TEXT,
    refund_id UUID REFERENCES gateway_refunds(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_refund_approval_payment_idempotency UNIQUE (payment_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_refund_approval_prop_status ON refund_approval_requests (property_id, status);
CREATE INDEX IF NOT EXISTS idx_refund_approval_payment ON refund_approval_requests (payment_id);

ALTER TABLE property_onboarding_settings
    ADD COLUMN IF NOT EXISTS single_owner_refund_threshold_paise BIGINT;
