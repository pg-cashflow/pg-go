-- Migration 047: PostgreSQL Row Level Security (RLS) for Financial Tables
--
-- Fulfills:
-- REQ-SEC-002 (P1-13): Multi-tenant defense-in-depth via database-enforced row level security
-- Ensures cross-property queries are strictly blocked at the SQL engine level.

-- Enable Row Level Security on core property-scoped financial and tenant tables
ALTER TABLE tenants ENABLE ROW LEVEL SECURITY;
ALTER TABLE dues ENABLE ROW LEVEL SECURITY;
ALTER TABLE payments ENABLE ROW LEVEL SECURITY;
ALTER TABLE expenses ENABLE ROW LEVEL SECURITY;
ALTER TABLE payout_payees ENABLE ROW LEVEL SECURITY;

DO $$ 
BEGIN
  -- 1. Tenants policy
  IF NOT EXISTS (
    SELECT 1 FROM pg_policies WHERE tablename = 'tenants' AND policyname = 'tenant_property_isolation'
  ) THEN
    CREATE POLICY tenant_property_isolation ON tenants
      FOR ALL
      USING (
        current_setting('app.current_property_id', true) IS NULL 
        OR current_setting('app.current_property_id', true) = '' 
        OR property_id = NULLIF(current_setting('app.current_property_id', true), '')::UUID
      )
      WITH CHECK (
        current_setting('app.current_property_id', true) IS NULL 
        OR current_setting('app.current_property_id', true) = '' 
        OR property_id = NULLIF(current_setting('app.current_property_id', true), '')::UUID
      );
  END IF;

  -- 2. Dues policy
  IF NOT EXISTS (
    SELECT 1 FROM pg_policies WHERE tablename = 'dues' AND policyname = 'due_property_isolation'
  ) THEN
    CREATE POLICY due_property_isolation ON dues
      FOR ALL
      USING (
        current_setting('app.current_property_id', true) IS NULL 
        OR current_setting('app.current_property_id', true) = '' 
        OR property_id = NULLIF(current_setting('app.current_property_id', true), '')::UUID
      )
      WITH CHECK (
        current_setting('app.current_property_id', true) IS NULL 
        OR current_setting('app.current_property_id', true) = '' 
        OR property_id = NULLIF(current_setting('app.current_property_id', true), '')::UUID
      );
  END IF;

  -- 3. Payments policy
  IF NOT EXISTS (
    SELECT 1 FROM pg_policies WHERE tablename = 'payments' AND policyname = 'payment_property_isolation'
  ) THEN
    CREATE POLICY payment_property_isolation ON payments
      FOR ALL
      USING (
        current_setting('app.current_property_id', true) IS NULL 
        OR current_setting('app.current_property_id', true) = '' 
        OR property_id = NULLIF(current_setting('app.current_property_id', true), '')::UUID
      )
      WITH CHECK (
        current_setting('app.current_property_id', true) IS NULL 
        OR current_setting('app.current_property_id', true) = '' 
        OR property_id = NULLIF(current_setting('app.current_property_id', true), '')::UUID
      );
  END IF;

  -- 4. Expenses policy
  IF NOT EXISTS (
    SELECT 1 FROM pg_policies WHERE tablename = 'expenses' AND policyname = 'expense_property_isolation'
  ) THEN
    CREATE POLICY expense_property_isolation ON expenses
      FOR ALL
      USING (
        current_setting('app.current_property_id', true) IS NULL 
        OR current_setting('app.current_property_id', true) = '' 
        OR property_id = NULLIF(current_setting('app.current_property_id', true), '')::UUID
      )
      WITH CHECK (
        current_setting('app.current_property_id', true) IS NULL 
        OR current_setting('app.current_property_id', true) = '' 
        OR property_id = NULLIF(current_setting('app.current_property_id', true), '')::UUID
      );
  END IF;

  -- 5. Payout Payees policy
  IF NOT EXISTS (
    SELECT 1 FROM pg_policies WHERE tablename = 'payout_payees' AND policyname = 'payout_payee_property_isolation'
  ) THEN
    CREATE POLICY payout_payee_property_isolation ON payout_payees
      FOR ALL
      USING (
        current_setting('app.current_property_id', true) IS NULL 
        OR current_setting('app.current_property_id', true) = '' 
        OR property_id = NULLIF(current_setting('app.current_property_id', true), '')::UUID
      )
      WITH CHECK (
        current_setting('app.current_property_id', true) IS NULL 
        OR current_setting('app.current_property_id', true) = '' 
        OR property_id = NULLIF(current_setting('app.current_property_id', true), '')::UUID
      );
  END IF;
END $$;
