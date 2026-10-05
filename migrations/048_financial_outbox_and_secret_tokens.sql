-- Migration 048: Financial Outbox & Secret Tokens & Period Transaction Locks
--
-- 1. Period serialization advisory lock and closed period enforcement on money tables
-- 2. Property calendar secret tokens table

CREATE OR REPLACE FUNCTION trg_money_period_lock_and_serialize() RETURNS trigger AS $$
DECLARE
  v_prop UUID;
  v_time TIMESTAMPTZ;
  v_period TEXT;
BEGIN
  IF TG_TABLE_NAME = 'payments' THEN
    v_prop := NEW.property_id;
    v_time := NEW.payment_date;
  ELSIF TG_TABLE_NAME = 'gateway_refunds' THEN
    v_prop := NEW.property_id;
    v_time := NEW.created_at;
  ELSIF TG_TABLE_NAME = 'refund_allocations' THEN
    SELECT r.property_id, r.created_at INTO v_prop, v_time
      FROM gateway_refunds r WHERE r.id = NEW.refund_id;
  ELSIF TG_TABLE_NAME = 'financial_corrections' THEN
    v_prop := NEW.property_id;
    v_time := NEW.created_at;
  END IF;

  IF v_prop IS NOT NULL AND v_time IS NOT NULL THEN
    v_period := to_char(v_time AT TIME ZONE 'UTC', 'YYYY-MM');

    -- Transaction advisory lock: serializes concurrent writes with period close
    PERFORM pg_advisory_xact_lock(hashtext(format('ledger-period:%s:%s', v_prop, v_period)));

    -- Closed period check
    IF EXISTS (
      SELECT 1 FROM period_tie_outs t
       WHERE t.property_id  = v_prop
         AND t.period_month = v_period
         AND t.status       = 'closed'
    ) THEN
      IF COALESCE(current_setting('app.reopen_period', true), 'off') <> 'on' THEN
        RAISE EXCEPTION 'period % is closed for property %; cannot modify %',
          v_period, v_prop, TG_TABLE_NAME
          USING ERRCODE = 'LG001';
      END IF;
    END IF;
  END IF;

  RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_payments_period_lock ON payments;
CREATE TRIGGER trg_payments_period_lock
  BEFORE INSERT OR UPDATE ON payments
  FOR EACH ROW EXECUTE FUNCTION trg_money_period_lock_and_serialize();

DROP TRIGGER IF EXISTS trg_gateway_refunds_period_lock ON gateway_refunds;
CREATE TRIGGER trg_gateway_refunds_period_lock
  BEFORE INSERT OR UPDATE ON gateway_refunds
  FOR EACH ROW EXECUTE FUNCTION trg_money_period_lock_and_serialize();

DROP TRIGGER IF EXISTS trg_refund_allocations_period_lock ON refund_allocations;
CREATE TRIGGER trg_refund_allocations_period_lock
  BEFORE INSERT OR UPDATE ON refund_allocations
  FOR EACH ROW EXECUTE FUNCTION trg_money_period_lock_and_serialize();

DROP TRIGGER IF EXISTS trg_financial_corrections_period_lock ON financial_corrections;
CREATE TRIGGER trg_financial_corrections_period_lock
  BEFORE INSERT OR UPDATE ON financial_corrections
  FOR EACH ROW EXECUTE FUNCTION trg_money_period_lock_and_serialize();

-- Calendar secret tokens table: stores hashed opaque tokens instead of deterministic HMAC
CREATE TABLE IF NOT EXISTS property_calendar_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_by UUID
);
CREATE INDEX IF NOT EXISTS idx_calendar_tokens_hash ON property_calendar_tokens (token_hash);
CREATE INDEX IF NOT EXISTS idx_calendar_tokens_prop ON property_calendar_tokens (property_id);
