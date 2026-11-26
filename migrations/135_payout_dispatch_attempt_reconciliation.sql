-- Migration 135: Durable payout dispatch attempt marker and bounded reconciliation.
-- A worker must commit dispatch_attempt_opened_at before sending the Cashfree batch POST.
-- Existing in-flight rows are backfilled as possibly submitted because the old schema cannot
-- prove whether a worker crashed before or after the provider accepted the request.

SET LOCAL lock_timeout = '5s';

ALTER TABLE payout_batches
    ADD COLUMN IF NOT EXISTS dispatch_attempt_opened_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS dispatch_attempt_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS dispatch_reconciliation_attempts SMALLINT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS dispatch_last_reconciled_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS dispatch_review_required_at TIMESTAMPTZ;

DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'payout_batches_dispatch_attempt_count_check'
          AND conrelid = 'payout_batches'::regclass
    ) THEN
        ALTER TABLE payout_batches
            ADD CONSTRAINT payout_batches_dispatch_attempt_count_check
            CHECK (dispatch_attempt_count >= 0);
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'payout_batches_dispatch_reconciliation_attempts_check'
          AND conrelid = 'payout_batches'::regclass
    ) THEN
        ALTER TABLE payout_batches
            ADD CONSTRAINT payout_batches_dispatch_reconciliation_attempts_check
            CHECK (dispatch_reconciliation_attempts BETWEEN 0 AND 5);
    END IF;
END $$;

-- Backfill conservatively. For an in-flight batch created before this migration, the process
-- may have died after Cashfree accepted the POST but before local state was updated. Treat it as
-- possibly submitted; never let a 404 cause an automatic duplicate POST.
UPDATE payout_batches
SET dispatch_attempt_opened_at = COALESCE(dispatch_attempt_opened_at, updated_at, created_at),
    dispatch_attempt_count = GREATEST(dispatch_attempt_count, 1)
WHERE status IN ('processing', 'dispatch_unknown')
  AND dispatch_attempt_opened_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_payout_batches_dispatch_reconcile_due
    ON payout_batches ((COALESCE(dispatch_last_reconciled_at, updated_at)), updated_at)
    WHERE dispatch_review_required_at IS NULL
      AND dispatch_attempt_opened_at IS NOT NULL
      AND status IN ('processing', 'dispatch_unknown');

COMMENT ON COLUMN payout_batches.dispatch_attempt_opened_at IS
    'Non-null while a batch may have been submitted to Cashfree. Written before POST; cleared only for typed provider responses that prove non-acceptance.';
COMMENT ON COLUMN payout_batches.dispatch_attempt_count IS
    'Count of durable pre-dispatch attempt markers. Retained across known provider rejections for auditability.';
COMMENT ON COLUMN payout_batches.dispatch_reconciliation_attempts IS
    'Bounded consecutive provider status lookup failures; at five the batch requires manual review.';
COMMENT ON COLUMN payout_batches.dispatch_last_reconciled_at IS
    'Time of the latest provider status lookup outcome, used to throttle stale-batch reconciliation.';
COMMENT ON COLUMN payout_batches.dispatch_review_required_at IS
    'Non-null when automatic status reconciliation reached its bounded attempt limit; no automated resubmission or polling is permitted.';
