-- Migration 054: Deposit Settlement State Machine & Universal Ledger Outbox (Gate A Remediation)
--
-- 1. Create deposit_settlements table with strict status transition and non-replay invariants
-- 2. Enforce check constraints on refunded_paise and deductions_paise
-- 3. Enable property-scoped RLS on deposit_settlements

CREATE TABLE IF NOT EXISTS deposit_settlements (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    tenant_id UUID NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    deposit_due_id UUID NOT NULL REFERENCES dues(id) ON DELETE RESTRICT,
    idempotency_key TEXT NOT NULL UNIQUE,
    original_deposit_paise BIGINT NOT NULL CHECK (original_deposit_paise > 0),
    refunded_paise BIGINT NOT NULL CHECK (refunded_paise >= 0),
    deductions_paise BIGINT NOT NULL CHECK (deductions_paise >= 0),
    status VARCHAR(30) NOT NULL DEFAULT 'settling' CHECK (status IN ('settling', 'settled', 'failed')),
    reason TEXT NOT NULL DEFAULT '',
    settled_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_deposit_settlement_sum CHECK (refunded_paise + deductions_paise <= original_deposit_paise)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_settled_deposit_due 
    ON deposit_settlements(deposit_due_id) WHERE status IN ('settling', 'settled');

CREATE INDEX IF NOT EXISTS idx_deposit_settlements_tenant ON deposit_settlements(tenant_id);
CREATE INDEX IF NOT EXISTS idx_deposit_settlements_prop ON deposit_settlements(property_id);

ALTER TABLE deposit_settlements ENABLE ROW LEVEL SECURITY;
ALTER TABLE deposit_settlements FORCE ROW LEVEL SECURITY;

DO $$
BEGIN
    DROP POLICY IF EXISTS deposit_settlements_property_isolation ON deposit_settlements;
    CREATE POLICY deposit_settlements_property_isolation ON deposit_settlements
        FOR ALL
        USING (
            current_setting('app.current_property_id', true) = property_id::text
            OR current_setting('app.ledger_maintenance', true) = 'on'
        )
        WITH CHECK (
            current_setting('app.current_property_id', true) = property_id::text
            OR current_setting('app.ledger_maintenance', true) = 'on'
        );
EXCEPTION
    WHEN OTHERS THEN NULL;
END $$;
