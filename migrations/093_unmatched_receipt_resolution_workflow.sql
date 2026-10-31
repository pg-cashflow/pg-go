SET lock_timeout = '5s';

-- Migration 093: turn unmatched_gateway_receipts into an owner-resolvable "held receipt" queue.
-- Standard: ASD-STE100
--
-- WHY EXTEND, NOT A NEW TABLE:
--   Migration 020 already created unmatched_gateway_receipts with resolved_at, resolved_by,
--   resolution_action ('refunded' | 'applied_as_credit' | 'written_off') and resolution_notes.
--   A second table would duplicate those columns and split one concept in two.
--
-- WHAT IS MISSING TODAY (verified in code):
--   PaymentRepo.RecordUnmatchedReceipt inserts only provider, order, payment id, intent id,
--   amount, reason and payload. property_id, tenant_id and due_id are always NULL, so an owner
--   queue cannot be scoped to a property. Group 2 changes the INSERT to fill them.
--   This migration adds the columns and backfills existing rows where the link can be derived.
--
-- RLS IS DELIBERATELY NOT ENABLED HERE (see migration 094, after Group 2 ships):
--   With property_id NULL on every row, a property-scoped policy would block the webhook INSERT.
--   No endpoint reads this table yet, so there is no owner-facing exposure in the interim.

-- 1. Linkage and workflow columns (all additive, all nullable or defaulted).
ALTER TABLE unmatched_gateway_receipts
    ADD COLUMN IF NOT EXISTS tenant_id UUID REFERENCES tenants(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS payment_id UUID REFERENCES payments(id) ON DELETE RESTRICT,
    ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'held',
    ADD COLUMN IF NOT EXISTS resolution_refund_id UUID REFERENCES gateway_refunds(id) ON DELETE RESTRICT,
    ADD COLUMN IF NOT EXISTS resolution_idempotency_key VARCHAR(128),
    ADD COLUMN IF NOT EXISTS held_notified_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS resolution_notified_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- 2. Backfill status from the existing resolution columns before adding constraints.
UPDATE unmatched_gateway_receipts
SET status = 'resolved'
WHERE resolved_at IS NOT NULL AND status <> 'resolved';

-- 3. Backfill property, due and tenant from the payment intent where it is known.
--    Rows without an intent (unknown_order, payment_not_found) stay NULL by design.
UPDATE unmatched_gateway_receipts r
SET due_id      = COALESCE(r.due_id, d.id),
    property_id = COALESCE(r.property_id, d.property_id),
    tenant_id   = COALESCE(r.tenant_id, d.tenant_id)
FROM payment_intents pi
JOIN dues d ON d.id = pi.due_id
WHERE r.payment_intent_id = pi.id
  AND (r.property_id IS NULL OR r.tenant_id IS NULL OR r.due_id IS NULL);

-- 4. Link the unapplied payments row when the webhook already wrote one
--    (closed-dues branch writes payments.is_unapplied = TRUE with the same cf_payment_id).
UPDATE unmatched_gateway_receipts r
SET payment_id = p.id
FROM payments p
WHERE r.payment_id IS NULL
  AND p.is_unapplied = TRUE
  AND p.cf_payment_id = r.provider_payment_id
  AND r.provider = 'cashfree';

-- 5. Constraints. Status values and the resolved/status coupling.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_unmatched_status') THEN
        ALTER TABLE unmatched_gateway_receipts
            ADD CONSTRAINT chk_unmatched_status
            CHECK (status IN ('held', 'resolving', 'resolved'));
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_unmatched_resolved_coupling') THEN
        ALTER TABLE unmatched_gateway_receipts
            ADD CONSTRAINT chk_unmatched_resolved_coupling
            CHECK ((status = 'resolved') = (resolved_at IS NOT NULL));
    END IF;
    -- New resolutions must name an action. Legacy resolved rows may lack one, so NOT VALID.
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_unmatched_action_when_resolved') THEN
        ALTER TABLE unmatched_gateway_receipts
            ADD CONSTRAINT chk_unmatched_action_when_resolved
            CHECK (status <> 'resolved' OR resolution_action IS NOT NULL) NOT VALID;
    END IF;
    -- A refund resolution must point at its refund row.
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'chk_unmatched_refund_link') THEN
        ALTER TABLE unmatched_gateway_receipts
            ADD CONSTRAINT chk_unmatched_refund_link
            CHECK (resolution_action IS DISTINCT FROM 'refunded' OR resolution_refund_id IS NOT NULL) NOT VALID;
    END IF;
END $$;

-- 6. Indexes. One refund and one payments row can resolve at most one receipt.
CREATE UNIQUE INDEX IF NOT EXISTS uq_unmatched_resolution_refund
    ON unmatched_gateway_receipts(resolution_refund_id)
    WHERE resolution_refund_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS uq_unmatched_payment
    ON unmatched_gateway_receipts(payment_id)
    WHERE payment_id IS NOT NULL;

-- Owner queue: open items per property, oldest first.
CREATE INDEX IF NOT EXISTS idx_unmatched_queue
    ON unmatched_gateway_receipts(property_id, created_at)
    WHERE status <> 'resolved';

-- Notification sweep: held receipts whose tenant has not been told yet.
CREATE INDEX IF NOT EXISTS idx_unmatched_unnotified
    ON unmatched_gateway_receipts(created_at)
    WHERE status = 'held' AND held_notified_at IS NULL AND tenant_id IS NOT NULL;

