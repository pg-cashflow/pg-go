-- Migration 049: Fix money period lock trigger timestamp column for payments
-- payments uses matched_at / created_at (not payment_date).

CREATE OR REPLACE FUNCTION trg_money_period_lock_and_serialize() RETURNS trigger AS $$
DECLARE
  v_prop UUID;
  v_time TIMESTAMPTZ;
  v_period TEXT;
BEGIN
  IF TG_TABLE_NAME = 'payments' THEN
    v_prop := NEW.property_id;
    v_time := COALESCE(NEW.matched_at, NEW.created_at);
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
