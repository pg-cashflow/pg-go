SET lock_timeout = '5s';

-- Migration 123: durable recovery for superseded Cashfree orders.
-- A due remains reserved until Cashfree confirms that its order is terminal
-- and the final payment lookup finds no successful capture.

-- Keep paise values aligned with the int64 domain model.
ALTER TABLE payment_intents
    ALTER COLUMN amount_paise TYPE BIGINT USING amount_paise::BIGINT;
ALTER TABLE payment_intent_quarantine
    ALTER COLUMN amount_paise TYPE BIGINT USING amount_paise::BIGINT;

ALTER TABLE payment_intent_quarantine
    ADD COLUMN IF NOT EXISTS property_id UUID REFERENCES properties(id),
    ADD COLUMN IF NOT EXISTS due_ids UUID[] NOT NULL DEFAULT '{}'::uuid[],
    ADD COLUMN IF NOT EXISTS status VARCHAR(24) NOT NULL DEFAULT 'pending',
    ADD COLUMN IF NOT EXISTS next_check_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN IF NOT EXISTS attempt_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS terminal_confirmation_count SMALLINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_provider_status VARCHAR(32),
    ADD COLUMN IF NOT EXISTS last_error TEXT,
    ADD COLUMN IF NOT EXISTS last_attempt_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS resolved_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

ALTER TABLE payment_intent_quarantine
    DROP CONSTRAINT IF EXISTS chk_payment_intent_quarantine_terminal_confirmations;
ALTER TABLE payment_intent_quarantine
    ADD CONSTRAINT chk_payment_intent_quarantine_terminal_confirmations
    CHECK (terminal_confirmation_count BETWEEN 0 AND 2);

-- Preserve quarantine history. An intent cannot be deleted while its provider order
-- may still accept payment or while its recovery record remains unresolved.
ALTER TABLE payment_intent_quarantine
    DROP CONSTRAINT IF EXISTS payment_intent_quarantine_payment_intent_id_fkey;
ALTER TABLE payment_intent_quarantine
    ADD CONSTRAINT payment_intent_quarantine_payment_intent_id_fkey
    FOREIGN KEY (payment_intent_id) REFERENCES payment_intents(id) ON DELETE RESTRICT;

ALTER TABLE payment_intent_quarantine
    DROP CONSTRAINT IF EXISTS chk_payment_intent_quarantine_status;
ALTER TABLE payment_intent_quarantine
    ADD CONSTRAINT chk_payment_intent_quarantine_status
    CHECK (status IN ('pending', 'termination_requested', 'needs_review', 'resolved'));

-- Refuse migration when legacy records cannot be scoped safely. Do not guess a
-- property or release a due when an order spans multiple properties.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1
        FROM payment_intent_quarantine q
        LEFT JOIN payment_intents pi ON pi.id = q.payment_intent_id
        LEFT JOIN LATERAL (
            SELECT q.due_id AS due_id WHERE q.due_id IS NOT NULL
            UNION
            SELECT pid.due_id
            FROM payment_intent_dues pid
            WHERE pid.payment_intent_id = q.payment_intent_id
        ) linked ON TRUE
        LEFT JOIN dues d ON d.id = linked.due_id
        GROUP BY q.payment_intent_id
        HAVING COUNT(DISTINCT d.id) = 0
            OR COUNT(DISTINCT d.property_id) <> 1
            OR COUNT(DISTINCT d.tenant_id) <> 1
    ) THEN
        RAISE EXCEPTION 'migration 123 blocked: existing payment_intent_quarantine rows cannot be scoped to one property and at least one due';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM payment_intent_quarantine
        WHERE BTRIM(COALESCE(provider_order_id, '')) = '' OR amount_paise <= 0
    ) THEN
        RAISE EXCEPTION 'migration 123 blocked: a legacy payment_intent_quarantine row has an invalid provider order ID or amount';
    END IF;

    IF EXISTS (
        SELECT 1
        FROM payment_intent_quarantine
        GROUP BY payment_intent_id
        HAVING COUNT(*) > 1
    ) THEN
        RAISE EXCEPTION 'migration 123 blocked: duplicate quarantine rows exist for a payment intent; reconcile them before migration';
    END IF;

    IF EXISTS (
        SELECT pi.id
        FROM payment_intents pi
        LEFT JOIN LATERAL (
            SELECT pi.due_id AS due_id WHERE pi.due_id IS NOT NULL
            UNION
            SELECT pid.due_id
            FROM payment_intent_dues pid
            WHERE pid.payment_intent_id = pi.id
        ) linked ON TRUE
        LEFT JOIN dues d ON d.id = linked.due_id
        WHERE pi.provider = 'cashfree'
          AND (
              pi.status = 'superseded'
              OR (pi.status = 'paid' AND EXISTS (
                  SELECT 1 FROM unmatched_gateway_receipts ur
                  WHERE (ur.payment_intent_id = pi.id OR ur.provider_order_id = pi.provider_order_id)
                    AND ur.status <> 'resolved'
              ))
          )
          AND BTRIM(COALESCE(pi.provider_order_id, '')) <> ''
        GROUP BY pi.id
        HAVING COUNT(DISTINCT d.id) = 0
            OR COUNT(DISTINCT d.property_id) <> 1
            OR COUNT(DISTINCT d.tenant_id) <> 1
    ) THEN
        RAISE EXCEPTION 'migration 123 blocked: a superseded Cashfree order has no due or spans multiple properties or tenants; reconcile it manually before migration';
    END IF;
END;
$$;

-- Populate the full due snapshot and property scope for older audit rows.
WITH legacy_due_set AS (
    SELECT
        q.id,
        ARRAY_AGG(DISTINCT d.id ORDER BY d.id) AS due_ids,
        (ARRAY_AGG(DISTINCT d.id ORDER BY d.id))[1] AS primary_due_id,
        MIN(d.property_id::text)::uuid AS property_id
    FROM payment_intent_quarantine q
    JOIN LATERAL (
        SELECT q.due_id AS due_id WHERE q.due_id IS NOT NULL
        UNION
        SELECT pid.due_id
        FROM payment_intent_dues pid
        WHERE pid.payment_intent_id = q.payment_intent_id
    ) linked ON TRUE
    JOIN dues d ON d.id = linked.due_id
    GROUP BY q.id
)
UPDATE payment_intent_quarantine q
SET due_id = s.primary_due_id,
    due_ids = s.due_ids,
    property_id = s.property_id,
    next_check_at = NOW(),
    updated_at = NOW()
FROM legacy_due_set s
WHERE q.id = s.id;

ALTER TABLE payment_intent_quarantine
    ALTER COLUMN due_id SET NOT NULL,
    ALTER COLUMN property_id SET NOT NULL;
ALTER TABLE payment_intent_quarantine
    DROP CONSTRAINT IF EXISTS chk_payment_intent_quarantine_due_ids;
ALTER TABLE payment_intent_quarantine
    ADD CONSTRAINT chk_payment_intent_quarantine_due_ids
    CHECK (CARDINALITY(due_ids) > 0);
ALTER TABLE payment_intent_quarantine
    DROP CONSTRAINT IF EXISTS chk_payment_intent_quarantine_amount;
ALTER TABLE payment_intent_quarantine
    ADD CONSTRAINT chk_payment_intent_quarantine_amount
    CHECK (amount_paise > 0 AND BTRIM(provider_order_id) <> '');

-- One provider order can affect several dues. Store one queue row per order and
-- retain every due in due_ids so checkout locks all of them until resolution.
CREATE UNIQUE INDEX IF NOT EXISTS uq_payment_intent_quarantine_intent
    ON payment_intent_quarantine (payment_intent_id);
CREATE INDEX IF NOT EXISTS idx_payment_intent_quarantine_due_ids
    ON payment_intent_quarantine USING GIN (due_ids);
CREATE INDEX IF NOT EXISTS idx_payment_intent_quarantine_next_check
    ON payment_intent_quarantine (next_check_at, created_at)
    WHERE status <> 'resolved';

-- Enable tenant isolation for app requests. Workers use the dedicated service
-- role and can claim records across properties.
ALTER TABLE payment_intent_quarantine ENABLE ROW LEVEL SECURITY;
ALTER TABLE payment_intent_quarantine FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS payment_intent_quarantine_app ON payment_intent_quarantine;
CREATE POLICY payment_intent_quarantine_app ON payment_intent_quarantine
    FOR ALL TO pgapp_app
    USING (
        property_id = CASE
            WHEN NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL
            THEN current_setting('app.current_property_id', true)::uuid
            ELSE NULL
        END
    )
    WITH CHECK (
        property_id = CASE
            WHEN NULLIF(current_setting('app.current_property_id', true), '') IS NOT NULL
            THEN current_setting('app.current_property_id', true)::uuid
            ELSE NULL
        END
    );

DROP POLICY IF EXISTS payment_intent_quarantine_service ON payment_intent_quarantine;
CREATE POLICY payment_intent_quarantine_service ON payment_intent_quarantine
    FOR ALL TO pgapp_maint, pgapp_worker
    USING (true)
    WITH CHECK (true);

GRANT SELECT, INSERT, UPDATE ON payment_intent_quarantine TO pgapp_app, pgapp_maint, pgapp_worker;
GRANT USAGE, SELECT ON SEQUENCE payment_intent_quarantine_id_seq TO pgapp_app, pgapp_maint, pgapp_worker;

-- Backfill superseded Cashfree orders and paid orders with unresolved receipts.
-- Keep each due reserved until provider and owner-resolution checks complete.
WITH intent_due_set AS (
    SELECT
        pi.id AS payment_intent_id,
        pi.provider_order_id,
        pi.amount_paise,
        pi.status AS original_status,
        ARRAY_AGG(DISTINCT d.id ORDER BY d.id) AS due_ids,
        (ARRAY_AGG(DISTINCT d.id ORDER BY d.id))[1] AS primary_due_id,
        (ARRAY_AGG(DISTINCT d.property_id ORDER BY d.property_id))[1] AS property_id
    FROM payment_intents pi
    JOIN LATERAL (
        SELECT pi.due_id AS due_id WHERE pi.due_id IS NOT NULL
        UNION
        SELECT pid.due_id FROM payment_intent_dues pid WHERE pid.payment_intent_id = pi.id
    ) linked ON true
    JOIN dues d ON d.id = linked.due_id
    WHERE pi.provider = 'cashfree'
      AND (
          pi.status = 'superseded'
          OR (pi.status = 'paid' AND EXISTS (
              SELECT 1 FROM unmatched_gateway_receipts ur
              WHERE (ur.payment_intent_id = pi.id OR ur.provider_order_id = pi.provider_order_id)
                    AND ur.status <> 'resolved'
          ))
      )
      AND BTRIM(COALESCE(pi.provider_order_id, '')) <> ''
    GROUP BY pi.id, pi.provider_order_id, pi.amount_paise, pi.status
)
INSERT INTO payment_intent_quarantine (
    payment_intent_id, due_id, property_id, due_ids, provider_order_id,
    amount_paise, original_status, quarantine_reason, status, next_check_at
)
SELECT
    payment_intent_id, primary_due_id, property_id, due_ids, provider_order_id,
    amount_paise, original_status,
    'Backfilled superseded Cashfree order; provider terminal state not yet verified',
    'pending', NOW()
FROM intent_due_set
ON CONFLICT (payment_intent_id) DO NOTHING;

-- Do not auto-resolve historical quarantine rows from local receipt state alone.
-- The worker must read Cashfree and persist PAID, EXPIRED, or TERMINATED first.

-- Queue newly superseded Cashfree orders in the same transaction as the state
-- transition. The trigger stores one row per provider order, including all dues.
CREATE OR REPLACE FUNCTION enqueue_superseded_cashfree_intent()
RETURNS TRIGGER
LANGUAGE plpgsql
SET search_path = public, pg_temp
AS $$
DECLARE
    v_due_ids UUID[];
    v_primary_due_id UUID;
    v_property_id UUID;
    v_property_count BIGINT;
    v_tenant_count BIGINT;
BEGIN
    IF NEW.provider <> 'cashfree'
       OR NEW.status <> 'superseded'
       OR OLD.status = 'superseded' THEN
        RETURN NEW;
    END IF;

    SELECT
        ARRAY_AGG(DISTINCT linked.due_id ORDER BY linked.due_id),
        COUNT(DISTINCT d.property_id),
        COUNT(DISTINCT d.tenant_id)
      INTO v_due_ids, v_property_count, v_tenant_count
    FROM (
        SELECT NEW.due_id AS due_id WHERE NEW.due_id IS NOT NULL
        UNION
        SELECT pid.due_id
        FROM payment_intent_dues pid
        WHERE pid.payment_intent_id = NEW.id
    ) linked
    JOIN dues d ON d.id = linked.due_id;

    IF BTRIM(COALESCE(NEW.provider_order_id, '')) = '' OR NEW.amount_paise <= 0 THEN
        RAISE EXCEPTION 'cannot quarantine superseded Cashfree intent % without a provider order ID and positive amount', NEW.id;
    END IF;
    IF v_due_ids IS NULL OR CARDINALITY(v_due_ids) = 0 THEN
        RAISE EXCEPTION 'cannot quarantine superseded Cashfree intent % without linked dues', NEW.id;
    END IF;
    IF v_property_count <> 1 OR v_tenant_count <> 1 THEN
        RAISE EXCEPTION 'cannot quarantine superseded Cashfree intent % because linked dues span % properties and % tenants', NEW.id, v_property_count, v_tenant_count;
    END IF;

    v_primary_due_id := v_due_ids[1];
    SELECT property_id INTO v_property_id FROM dues WHERE id = v_primary_due_id;
    IF v_property_id IS NULL THEN
        RAISE EXCEPTION 'cannot resolve property for superseded Cashfree intent %', NEW.id;
    END IF;

    INSERT INTO payment_intent_quarantine (
        payment_intent_id, due_id, property_id, due_ids, provider_order_id,
        amount_paise, original_status, quarantine_reason, status, next_check_at
    ) VALUES (
        NEW.id, v_primary_due_id, v_property_id, v_due_ids, NEW.provider_order_id,
        NEW.amount_paise, OLD.status,
        'Cashfree order superseded locally; gateway terminal state must be verified',
        'pending', NOW()
    )
    ON CONFLICT (payment_intent_id) DO NOTHING;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_enqueue_superseded_cashfree_intent ON payment_intents;
CREATE TRIGGER trg_enqueue_superseded_cashfree_intent
AFTER UPDATE OF status ON payment_intents
FOR EACH ROW
WHEN (NEW.status = 'superseded' AND OLD.status <> 'superseded' AND NEW.provider = 'cashfree')
EXECUTE FUNCTION enqueue_superseded_cashfree_intent();
