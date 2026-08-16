-- Allow multiple payments per due (UPI partials); property-scoped events without tenant.

ALTER TABLE payments DROP CONSTRAINT IF EXISTS uq_payment_per_due;
CREATE INDEX IF NOT EXISTS idx_payments_due ON payments(due_id);

ALTER TABLE events ALTER COLUMN tenant_id DROP NOT NULL;
DROP INDEX IF EXISTS idx_events_tenant_type;
CREATE INDEX idx_events_tenant_type ON events(tenant_id, event_type) WHERE tenant_id IS NOT NULL;
