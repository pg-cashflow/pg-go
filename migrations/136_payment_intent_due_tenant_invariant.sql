SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';

-- Migration 136: Single-tenant invariant for payment intent dues.
-- Standard: ASD-STE100.
-- Every due linked to a payment intent must belong to the same tenant.
-- Migration 134 enforces property equality. This migration enforces tenant equality.

DO $$
DECLARE
    cross_tenant_count BIGINT;
    sample_intent_id TEXT;
BEGIN
    WITH intent_tenants AS (
        SELECT pi.id AS payment_intent_id, d.tenant_id
        FROM payment_intents pi
        JOIN dues d ON d.id = pi.due_id
        WHERE pi.due_id IS NOT NULL

        UNION ALL

        SELECT pid.payment_intent_id, d.tenant_id
        FROM payment_intent_dues pid
        JOIN dues d ON d.id = pid.due_id
    ), mismatched AS (
        SELECT payment_intent_id
        FROM intent_tenants
        GROUP BY payment_intent_id
        HAVING COUNT(DISTINCT tenant_id) > 1
    )
    SELECT COUNT(*), MIN(payment_intent_id::TEXT)
    INTO cross_tenant_count, sample_intent_id
    FROM mismatched;

    IF cross_tenant_count > 0 THEN
        RAISE EXCEPTION
            'Migration 136 found % payment_intents that link dues from multiple tenants (sample id: %). Resolve the inconsistent links before retrying.',
            cross_tenant_count, sample_intent_id;
    END IF;
END $$;

-- Reject any payment_intent_dues row whose due belongs to a different tenant than
-- the parent due_id or any existing sibling due on the same payment intent.
CREATE OR REPLACE FUNCTION enforce_payment_intent_due_tenant()
RETURNS TRIGGER AS $$
DECLARE
    new_tenant_id UUID;
    parent_due_id UUID;
    parent_tenant_id UUID;
    sibling_tenant_id UUID;
BEGIN
    SELECT pi.due_id, d.tenant_id
    INTO parent_due_id, new_tenant_id
    FROM payment_intents pi
    JOIN dues d ON d.id = NEW.due_id
    WHERE pi.id = NEW.payment_intent_id
    FOR UPDATE OF pi, d;

    IF new_tenant_id IS NULL THEN
        RAISE EXCEPTION 'payment_intent_due requires an existing parent intent and due with a tenant_id';
    END IF;

    IF parent_due_id IS NOT NULL THEN
        SELECT d.tenant_id INTO parent_tenant_id
        FROM dues d
        WHERE d.id = parent_due_id;

        IF parent_tenant_id IS NOT NULL AND parent_tenant_id <> new_tenant_id THEN
            RAISE EXCEPTION 'payment_intent % due % tenant % does not match parent due % tenant %',
                NEW.payment_intent_id, NEW.due_id, new_tenant_id, parent_due_id, parent_tenant_id;
        END IF;
    END IF;

    SELECT d.tenant_id INTO sibling_tenant_id
    FROM payment_intent_dues pid
    JOIN dues d ON d.id = pid.due_id
    WHERE pid.payment_intent_id = NEW.payment_intent_id
      AND pid.id IS DISTINCT FROM NEW.id
      AND d.tenant_id <> new_tenant_id
    LIMIT 1;

    IF sibling_tenant_id IS NOT NULL THEN
        RAISE EXCEPTION 'payment_intent % cannot link due % for tenant %; existing linked dues belong to tenant %',
            NEW.payment_intent_id, NEW.due_id, new_tenant_id, sibling_tenant_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_payment_intent_due_tenant ON payment_intent_dues;
CREATE TRIGGER trg_payment_intent_due_tenant
BEFORE INSERT OR UPDATE OF payment_intent_id, due_id
ON payment_intent_dues
FOR EACH ROW
EXECUTE FUNCTION enforce_payment_intent_due_tenant();

-- Reject updating payment_intents.due_id to a due owned by a different tenant
-- than existing child rows in payment_intent_dues.
CREATE OR REPLACE FUNCTION enforce_payment_intent_parent_due_tenant()
RETURNS TRIGGER AS $$
DECLARE
    new_tenant_id UUID;
    child_tenant_id UUID;
BEGIN
    IF NEW.due_id IS NULL THEN
        RETURN NEW;
    END IF;

    SELECT d.tenant_id INTO new_tenant_id
    FROM dues d
    WHERE d.id = NEW.due_id;

    IF new_tenant_id IS NULL THEN
        RETURN NEW;
    END IF;

    SELECT d.tenant_id INTO child_tenant_id
    FROM payment_intent_dues pid
    JOIN dues d ON d.id = pid.due_id
    WHERE pid.payment_intent_id = NEW.id
      AND d.tenant_id <> new_tenant_id
    LIMIT 1;

    IF child_tenant_id IS NOT NULL THEN
        RAISE EXCEPTION 'payment_intent % due_id % tenant % does not match linked child due tenant %',
            NEW.id, NEW.due_id, new_tenant_id, child_tenant_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_payment_intent_parent_due_tenant ON payment_intents;
CREATE TRIGGER trg_payment_intent_parent_due_tenant
BEFORE INSERT OR UPDATE OF due_id
ON payment_intents
FOR EACH ROW
EXECUTE FUNCTION enforce_payment_intent_parent_due_tenant();
