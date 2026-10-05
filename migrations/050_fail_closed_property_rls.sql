-- Migration 050: Fail-Closed PostgreSQL Row Level Security (RLS)
--
-- Replaces fail-open RLS policies with strict fail-closed enforcement:
-- 1. FORCE ROW LEVEL SECURITY on all 5 protected tables
-- 2. Reject access when app.current_property_id is missing or empty
-- 3. Dedicated maintenance override via app.ledger_maintenance = 'on'

ALTER TABLE tenants FORCE ROW LEVEL SECURITY;
ALTER TABLE dues FORCE ROW LEVEL SECURITY;
ALTER TABLE payments FORCE ROW LEVEL SECURITY;
ALTER TABLE expenses FORCE ROW LEVEL SECURITY;
ALTER TABLE payout_payees FORCE ROW LEVEL SECURITY;

-- 1. Tenants policy
DROP POLICY IF EXISTS tenant_property_isolation ON tenants;
CREATE POLICY tenant_property_isolation ON tenants
  FOR ALL
  USING (
    (NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
     AND property_id = current_setting('app.current_property_id', true)::UUID)
    OR current_setting('app.ledger_maintenance', true) = 'on'
  )
  WITH CHECK (
    (NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
     AND property_id = current_setting('app.current_property_id', true)::UUID)
    OR current_setting('app.ledger_maintenance', true) = 'on'
  );

-- 2. Dues policy
DROP POLICY IF EXISTS due_property_isolation ON dues;
CREATE POLICY due_property_isolation ON dues
  FOR ALL
  USING (
    (NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
     AND property_id = current_setting('app.current_property_id', true)::UUID)
    OR current_setting('app.ledger_maintenance', true) = 'on'
  )
  WITH CHECK (
    (NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
     AND property_id = current_setting('app.current_property_id', true)::UUID)
    OR current_setting('app.ledger_maintenance', true) = 'on'
  );

-- 3. Payments policy
DROP POLICY IF EXISTS payment_property_isolation ON payments;
CREATE POLICY payment_property_isolation ON payments
  FOR ALL
  USING (
    (NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
     AND property_id = current_setting('app.current_property_id', true)::UUID)
    OR current_setting('app.ledger_maintenance', true) = 'on'
  )
  WITH CHECK (
    (NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
     AND property_id = current_setting('app.current_property_id', true)::UUID)
    OR current_setting('app.ledger_maintenance', true) = 'on'
  );

-- 4. Expenses policy
DROP POLICY IF EXISTS expense_property_isolation ON expenses;
CREATE POLICY expense_property_isolation ON expenses
  FOR ALL
  USING (
    (NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
     AND property_id = current_setting('app.current_property_id', true)::UUID)
    OR current_setting('app.ledger_maintenance', true) = 'on'
  )
  WITH CHECK (
    (NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
     AND property_id = current_setting('app.current_property_id', true)::UUID)
    OR current_setting('app.ledger_maintenance', true) = 'on'
  );

-- 5. Payout Payees policy
DROP POLICY IF EXISTS payout_payee_property_isolation ON payout_payees;
CREATE POLICY payout_payee_property_isolation ON payout_payees
  FOR ALL
  USING (
    (NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
     AND property_id = current_setting('app.current_property_id', true)::UUID)
    OR current_setting('app.ledger_maintenance', true) = 'on'
  )
  WITH CHECK (
    (NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
     AND property_id = current_setting('app.current_property_id', true)::UUID)
    OR current_setting('app.ledger_maintenance', true) = 'on'
  );
