-- 111: a tenant name change voids the verified deposit-refund payee.
-- The payee was penny-dropped and name-matched against the old name. Dispatch
-- re-checks is_verified (payout_dispatcher.go), so in-flight batches fail closed
-- until the tenant re-verifies. Invoker rights on purpose: payout_payees is under
-- FORCE RLS, and the editing session already carries the property scope.
CREATE OR REPLACE FUNCTION trg_tenant_name_voids_refund_payee() RETURNS trigger AS $$
BEGIN
  IF NEW.name IS DISTINCT FROM OLD.name THEN
    UPDATE payout_payees
       SET is_verified = FALSE,
           verification_status = 'unverified',
           verified_by = NULL,
           verified_at = NULL,
           updated_at = NOW()
     WHERE tenant_id = NEW.id
       AND payee_type = 'tenant_deposit'
       AND is_verified = TRUE;
  END IF;
  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_tenant_name_voids_refund_payee ON tenants;
CREATE TRIGGER trg_tenant_name_voids_refund_payee
  AFTER UPDATE OF name ON tenants
  FOR EACH ROW EXECUTE FUNCTION trg_tenant_name_voids_refund_payee();
