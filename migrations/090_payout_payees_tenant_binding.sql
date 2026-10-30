-- Migration 090: Bind payout_payees to tenant_id for deposit returns
-- Standard: ASD-STE100
-- Reference: ADR-009, ADR-022

-- 1. Add tenant_id column
ALTER TABLE payout_payees 
    ADD COLUMN IF NOT EXISTS tenant_id UUID REFERENCES tenants(id) ON DELETE RESTRICT;

CREATE INDEX IF NOT EXISTS idx_payout_payees_tenant 
    ON payout_payees (tenant_id) 
    WHERE tenant_id IS NOT NULL;

-- 2. Drop existing unique index that caused collisions for guardian with multiple minors
DROP INDEX IF EXISTS uq_payee_dedup;

-- 3. Create partial unique indexes
-- Split 1: Deposit payees scoped to (property_id, tenant_id, account_number_hash, payee_type)
CREATE UNIQUE INDEX IF NOT EXISTS uq_payee_tenant_deposit 
    ON payout_payees (property_id, tenant_id, account_number_hash, payee_type)
    WHERE payee_type IN ('tenant_deposit', 'guardian_deposit');

-- Split 2: General payees (vendors, staff) scoped to (property_id, account_number_hash, payee_type)
CREATE UNIQUE INDEX IF NOT EXISTS uq_payee_general 
    ON payout_payees (property_id, account_number_hash, payee_type)
    WHERE payee_type NOT IN ('tenant_deposit', 'guardian_deposit');

-- 4. Clean orphan dev/test payees without tenant_id that would fail validation
DELETE FROM payout_payees 
WHERE payee_type IN ('tenant_deposit', 'guardian_deposit') 
  AND tenant_id IS NULL
  AND NOT EXISTS (SELECT 1 FROM payout_items WHERE payout_items.payee_id = payout_payees.id);

-- 5. Add scoped check constraint for deposit payees
ALTER TABLE payout_payees ADD CONSTRAINT chk_payee_tenant_link
    CHECK (
        (payee_type IN ('tenant_deposit', 'guardian_deposit') AND tenant_id IS NOT NULL)
        OR (payee_type NOT IN ('tenant_deposit', 'guardian_deposit'))
    ) NOT VALID;

ALTER TABLE payout_payees VALIDATE CONSTRAINT chk_payee_tenant_link;

