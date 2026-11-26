SET LOCAL lock_timeout = '5s';
SET LOCAL statement_timeout = '60s';

-- Migration 134: Property ownership for payment intent parents.
-- Standard: ASD-STE100.
-- A payment intent can reserve one or several dues. Its ownership must not
-- depend on due_id, because due_id is NULL for a multi-due parent.

ALTER TABLE payment_intents
    ADD COLUMN IF NOT EXISTS property_id UUID;

-- Resolve historical ownership from every authoritative relationship.
-- Do not guess when the sources disagree or no source can identify the owner.
WITH linked_properties AS (
    SELECT pi.id AS payment_intent_id, d.property_id
    FROM payment_intents pi
    JOIN dues d ON d.id = pi.due_id

    UNION ALL

    SELECT pid.payment_intent_id, d.property_id
    FROM payment_intent_dues pid
    JOIN dues d ON d.id = pid.due_id

    UNION ALL

    SELECT pi.id, p.property_id
    FROM payment_intents pi
    JOIN payments p ON p.cf_payment_id = pi.cf_payment_id
    WHERE pi.cf_payment_id IS NOT NULL

    UNION ALL

    SELECT q.payment_intent_id, q.property_id
    FROM payment_intent_quarantine q
), resolved_properties AS (
    SELECT payment_intent_id,
           MIN(property_id::TEXT)::UUID AS property_id,
           COUNT(DISTINCT property_id) AS property_count
    FROM linked_properties
    GROUP BY payment_intent_id
)
UPDATE payment_intents pi
SET property_id = rp.property_id
FROM resolved_properties rp
WHERE rp.payment_intent_id = pi.id
  AND rp.property_count = 1
  AND pi.property_id IS NULL;

DO $$
DECLARE
    unresolved_count BIGINT;
    unresolved_sample TEXT;
    inconsistent_count BIGINT;
BEGIN
    SELECT COUNT(*), MIN(id::TEXT)
    INTO unresolved_count, unresolved_sample
    FROM payment_intents
    WHERE property_id IS NULL;

    IF unresolved_count > 0 THEN
        RAISE EXCEPTION
            'Migration 134 cannot resolve property ownership for % payment_intents (sample id: %). Resolve the source records before retrying.',
            unresolved_count, unresolved_sample;
    END IF;

    SELECT COUNT(*)
    INTO inconsistent_count
    FROM payment_intents pi
    WHERE (pi.due_id IS NOT NULL AND NOT EXISTS (
              SELECT 1 FROM dues d
              WHERE d.id = pi.due_id AND d.property_id = pi.property_id
          ))
       OR EXISTS (
              SELECT 1
              FROM payment_intent_dues pid
              JOIN dues d ON d.id = pid.due_id
              WHERE pid.payment_intent_id = pi.id
                AND d.property_id <> pi.property_id
          );

    IF inconsistent_count > 0 THEN
        RAISE EXCEPTION
            'Migration 134 found % payment_intents whose parent property differs from a linked due. Resolve the inconsistent links before retrying.',
            inconsistent_count;
    END IF;
END $$;

ALTER TABLE payment_intents
    ALTER COLUMN property_id SET NOT NULL;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'fk_payment_intents_property'
          AND conrelid = 'payment_intents'::regclass
    ) THEN
        ALTER TABLE payment_intents
            ADD CONSTRAINT fk_payment_intents_property
            FOREIGN KEY (property_id) REFERENCES properties(id) ON DELETE RESTRICT;
    END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_payment_intents_property_created
    ON payment_intents (property_id, created_at DESC);

-- Keep legacy single-due inserts working. Multi-due inserts must provide the
-- parent property explicitly or run in a property-scoped transaction.
CREATE OR REPLACE FUNCTION set_payment_intent_property_id()
RETURNS TRIGGER AS $$
DECLARE
    linked_property_id UUID;
    scoped_property_id UUID;
BEGIN
    IF NEW.due_id IS NOT NULL THEN
        SELECT d.property_id INTO linked_property_id
        FROM dues d
        WHERE d.id = NEW.due_id;

        IF linked_property_id IS NULL THEN
            RAISE EXCEPTION 'payment_intent due % has no resolvable property', NEW.due_id;
        END IF;

        IF NEW.property_id IS NULL THEN
            NEW.property_id := linked_property_id;
        ELSIF NEW.property_id <> linked_property_id THEN
            RAISE EXCEPTION 'payment_intent property % does not match due % property %',
                NEW.property_id, NEW.due_id, linked_property_id;
        END IF;
    END IF;

    IF NEW.property_id IS NULL THEN
        scoped_property_id := NULLIF(current_setting('app.current_property_id', true), '')::UUID;
        IF scoped_property_id IS NULL THEN
            RAISE EXCEPTION 'payment_intent property_id is required outside a property-scoped transaction';
        END IF;
        NEW.property_id := scoped_property_id;
    END IF;

    IF TG_OP = 'UPDATE' THEN
        IF NEW.property_id IS DISTINCT FROM OLD.property_id
           AND EXISTS (
               SELECT 1
               FROM payment_intent_dues pid
               JOIN dues d ON d.id = pid.due_id
               WHERE pid.payment_intent_id = NEW.id
                 AND d.property_id <> NEW.property_id
           ) THEN
            RAISE EXCEPTION 'payment_intent % cannot change property while linked dues belong to another property', NEW.id;
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_payment_intent_property_id ON payment_intents;
CREATE TRIGGER trg_payment_intent_property_id
BEFORE INSERT OR UPDATE OF property_id, due_id
ON payment_intents
FOR EACH ROW
EXECUTE FUNCTION set_payment_intent_property_id();

-- The parent and every child due must belong to the same property.
CREATE OR REPLACE FUNCTION enforce_payment_intent_due_property()
RETURNS TRIGGER AS $$
DECLARE
    parent_property_id UUID;
    due_property_id UUID;
BEGIN
    SELECT pi.property_id, d.property_id
    INTO parent_property_id, due_property_id
    FROM payment_intents pi
    JOIN dues d ON d.id = NEW.due_id
    WHERE pi.id = NEW.payment_intent_id
    FOR UPDATE OF pi, d;

    IF parent_property_id IS NULL OR due_property_id IS NULL THEN
        RAISE EXCEPTION 'payment_intent_due requires an existing parent intent and due';
    END IF;

    IF parent_property_id <> due_property_id THEN
        RAISE EXCEPTION 'payment_intent % property % does not match due % property %',
            NEW.payment_intent_id, parent_property_id, NEW.due_id, due_property_id;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_payment_intent_due_property ON payment_intent_dues;
CREATE TRIGGER trg_payment_intent_due_property
BEFORE INSERT OR UPDATE OF payment_intent_id, due_id
ON payment_intent_dues
FOR EACH ROW
EXECUTE FUNCTION enforce_payment_intent_due_property();

-- Replace the due-derived parent policy from migration 128. Child tables retain
-- their due-derived policies because each child row has one due_id.
DROP POLICY IF EXISTS payment_intents_app ON payment_intents;
CREATE POLICY payment_intents_app ON payment_intents
    FOR ALL TO pgapp_app
    USING (
        property_id = NULLIF(current_setting('app.current_property_id', true), '')::UUID
    )
    WITH CHECK (
        property_id = NULLIF(current_setting('app.current_property_id', true), '')::UUID
    );

COMMENT ON COLUMN payment_intents.property_id IS
    'Authoritative tenant scope for the payment intent parent. Required because one intent may cover multiple dues.';
