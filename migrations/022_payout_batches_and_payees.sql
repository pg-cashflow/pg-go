-- Migration 022: Operational Payouts Subsystem, Contractual Ceiling & Departure Settlements

-- 0. Contractual Ceiling on Dues to survive post-settlement zero-amount states
ALTER TABLE dues
    ADD COLUMN IF NOT EXISTS contractual_ceiling_paise INTEGER;

-- 1. Payees Supporting Bank Accounts & UPI-Only Payees (Reusable across siblings)
CREATE TABLE IF NOT EXISTS payout_payees (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    payee_type TEXT NOT NULL CHECK (payee_type IN ('staff', 'vendor', 'tenant_deposit', 'guardian_deposit')),
    name TEXT NOT NULL,
    phone TEXT,
    account_number_encrypted BYTEA,
    account_number_last4 CHAR(4),
    account_number_hash CHAR(64) NOT NULL, -- HMAC-SHA256 of account or UPI for dedup
    ifsc TEXT,
    bank_name TEXT,
    upi_vpa TEXT,
    key_version INT NOT NULL DEFAULT 1,
    is_verified BOOLEAN NOT NULL DEFAULT FALSE,
    verified_by UUID REFERENCES users(id),
    verified_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_payee_target CHECK (account_number_encrypted IS NOT NULL OR upi_vpa IS NOT NULL)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_payee_dedup 
    ON payout_payees (property_id, account_number_hash, payee_type);

-- 2. Tenant Departures (Constrained 1 Active per Tenant, 24h SLA Triggered at Inspection)
CREATE TABLE IF NOT EXISTS tenant_departures (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id UUID NOT NULL REFERENCES tenants(id),
    property_id UUID NOT NULL REFERENCES properties(id),
    notice_given_at TIMESTAMPTZ NOT NULL,
    planned_vacate_date DATE NOT NULL,
    actual_vacate_date DATE,
    inspected_at TIMESTAMPTZ, -- Room check & key handover timestamp (starts 24h SLA)
    sla_deadline_at TIMESTAMPTZ, -- inspected_at + 24 hours
    deposit_amount_paise BIGINT NOT NULL,
    unused_rent_refund_paise BIGINT NOT NULL DEFAULT 0,
    prorated_rent_owed_paise BIGINT NOT NULL DEFAULT 0,
    outstanding_dues_netted_paise BIGINT NOT NULL DEFAULT 0,
    deductions_paise BIGINT NOT NULL DEFAULT 0,
    net_refund_paise BIGINT NOT NULL CHECK (net_refund_paise >= 0),
    receivable_balance_paise BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'inspected', 'approved', 'refunded', 'cancelled')),
    notes TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_departure_rent_exclusion CHECK (NOT (unused_rent_refund_paise > 0 AND prorated_rent_owed_paise > 0))
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_tenant_one_active_departure 
    ON tenant_departures (tenant_id) WHERE status IN ('pending', 'inspected', 'approved');

-- 3. Itemized Departure Damages & Disputes Table
CREATE TABLE IF NOT EXISTS departure_deductions (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    departure_id UUID NOT NULL REFERENCES tenant_departures(id) ON DELETE CASCADE,
    description TEXT NOT NULL,
    amount_paise BIGINT NOT NULL CHECK (amount_paise > 0),
    evidence_photo_key TEXT, -- S3/R2 storage object key
    status TEXT NOT NULL DEFAULT 'agreed' CHECK (status IN ('agreed', 'disputed', 'waived')),
    tenant_acknowledged_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 4. Append-Only Departure Due Adjustments (Reversing prepaid rent without mutating historical payment_allocations)
CREATE TABLE IF NOT EXISTS departure_due_adjustments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    departure_id UUID NOT NULL REFERENCES tenant_departures(id) ON DELETE CASCADE,
    due_id UUID NOT NULL REFERENCES dues(id) ON DELETE RESTRICT,
    amount_paise BIGINT NOT NULL CHECK (amount_paise > 0),
    adjustment_type TEXT NOT NULL CHECK (adjustment_type IN ('unused_rent_reversal')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_departure_due_adjustment UNIQUE (departure_id, due_id)
);
CREATE INDEX IF NOT EXISTS idx_departure_due_adjustments_due ON departure_due_adjustments(due_id);

-- 5. Payout Batches (Owner-Only Approval Authority)
CREATE TABLE IF NOT EXISTS payout_batches (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    batch_number TEXT NOT NULL UNIQUE,
    format_type TEXT NOT NULL DEFAULT 'instruction_sheet',
    status TEXT NOT NULL DEFAULT 'draft' CHECK (status IN ('draft', 'approved', 'processing', 'completed', 'partially_failed', 'cancelled')),
    total_amount_paise BIGINT NOT NULL DEFAULT 0,
    item_count INT NOT NULL DEFAULT 0,
    created_by UUID NOT NULL REFERENCES users(id),
    approved_by UUID REFERENCES users(id),
    approved_at TIMESTAMPTZ,
    file_checksum CHAR(64),
    notes TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 6. Payout Items (Decoupled Treasury: unbatched until owner batch creation)
CREATE TABLE IF NOT EXISTS payout_items (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    batch_id UUID REFERENCES payout_batches(id) ON DELETE SET NULL, -- Nullable until batched by owner
    payee_id UUID NOT NULL REFERENCES payout_payees(id),
    departure_id UUID REFERENCES tenant_departures(id),
    reference_number VARCHAR(128) NOT NULL DEFAULT '',
    amount_paise BIGINT NOT NULL CHECK (amount_paise > 0),
    purpose TEXT NOT NULL,
    period_label TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'succeeded', 'failed', 'rejected', 'cancelled')),
    utr TEXT,
    settled_at TIMESTAMPTZ,
    failure_reason TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_payout_dedup UNIQUE (payee_id, purpose, period_label, reference_number)
);
CREATE INDEX IF NOT EXISTS idx_payout_items_batch ON payout_items(batch_id);
CREATE INDEX IF NOT EXISTS idx_payout_items_pending_unbatched ON payout_items(status) WHERE batch_id IS NULL AND status = 'pending';
CREATE UNIQUE INDEX IF NOT EXISTS uq_payout_departure_active 
    ON payout_items (departure_id) WHERE departure_id IS NOT NULL AND status IN ('pending', 'succeeded');
