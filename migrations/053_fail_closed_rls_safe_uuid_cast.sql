-- Migration 053: Safe Short-Circuiting UUID Cast in Fail-Closed Row Level Security Policies
--
-- Replaces non-short-circuiting AND cast with CASE WHEN ... THEN ...::UUID ELSE NULL END.
-- When app.current_property_id is unset or empty, CASE evaluates strictly to NULL,
-- evaluating property_id = NULL to FALSE/UNKNOWN without raising 22P02 invalid UUID syntax error.

-- 1. Tenants policy
DROP POLICY IF EXISTS tenant_property_isolation ON tenants;
CREATE POLICY tenant_property_isolation ON tenants
  FOR ALL
  USING (
    property_id = (
      CASE 
        WHEN NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
        THEN current_setting('app.current_property_id', true)::UUID 
        ELSE NULL 
      END
    )
    OR current_setting('app.ledger_maintenance', true) = 'on'
  )
  WITH CHECK (
    property_id = (
      CASE 
        WHEN NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
        THEN current_setting('app.current_property_id', true)::UUID 
        ELSE NULL 
      END
    )
    OR current_setting('app.ledger_maintenance', true) = 'on'
  );

-- 2. Dues policy
DROP POLICY IF EXISTS due_property_isolation ON dues;
CREATE POLICY due_property_isolation ON dues
  FOR ALL
  USING (
    property_id = (
      CASE 
        WHEN NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
        THEN current_setting('app.current_property_id', true)::UUID 
        ELSE NULL 
      END
    )
    OR current_setting('app.ledger_maintenance', true) = 'on'
  )
  WITH CHECK (
    property_id = (
      CASE 
        WHEN NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
        THEN current_setting('app.current_property_id', true)::UUID 
        ELSE NULL 
      END
    )
    OR current_setting('app.ledger_maintenance', true) = 'on'
  );

-- 3. Payments policy
DROP POLICY IF EXISTS payment_property_isolation ON payments;
CREATE POLICY payment_property_isolation ON payments
  FOR ALL
  USING (
    property_id = (
      CASE 
        WHEN NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
        THEN current_setting('app.current_property_id', true)::UUID 
        ELSE NULL 
      END
    )
    OR current_setting('app.ledger_maintenance', true) = 'on'
  )
  WITH CHECK (
    property_id = (
      CASE 
        WHEN NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
        THEN current_setting('app.current_property_id', true)::UUID 
        ELSE NULL 
      END
    )
    OR current_setting('app.ledger_maintenance', true) = 'on'
  );

-- 4. Expenses policy
DROP POLICY IF EXISTS expense_property_isolation ON expenses;
CREATE POLICY expense_property_isolation ON expenses
  FOR ALL
  USING (
    property_id = (
      CASE 
        WHEN NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
        THEN current_setting('app.current_property_id', true)::UUID 
        ELSE NULL 
      END
    )
    OR current_setting('app.ledger_maintenance', true) = 'on'
  )
  WITH CHECK (
    property_id = (
      CASE 
        WHEN NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
        THEN current_setting('app.current_property_id', true)::UUID 
        ELSE NULL 
      END
    )
    OR current_setting('app.ledger_maintenance', true) = 'on'
  );

-- 5. Payout Payees policy
DROP POLICY IF EXISTS payout_payee_property_isolation ON payout_payees;
CREATE POLICY payout_payee_property_isolation ON payout_payees
  FOR ALL
  USING (
    property_id = (
      CASE 
        WHEN NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
        THEN current_setting('app.current_property_id', true)::UUID 
        ELSE NULL 
      END
    )
    OR current_setting('app.ledger_maintenance', true) = 'on'
  )
  WITH CHECK (
    property_id = (
      CASE 
        WHEN NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL 
        THEN current_setting('app.current_property_id', true)::UUID 
        ELSE NULL 
      END
    )
    OR current_setting('app.ledger_maintenance', true) = 'on'
  );
