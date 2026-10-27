-- Migration 088: allow every processing_status value the application actually writes.
-- Previously 'duplicate' and 'ignored_unconfirmed' violated the CHECK from migration 020, and the
-- resulting UPDATE errors were discarded, leaving those events stuck at 'received' in the audit trail.
DO $$
DECLARE c TEXT;
BEGIN
    FOR c IN
        SELECT conname FROM pg_constraint
        WHERE conrelid = 'webhook_events'::regclass
          AND contype = 'c'
          AND pg_get_constraintdef(oid) ILIKE '%processing_status%'
    LOOP
        EXECUTE format('ALTER TABLE webhook_events DROP CONSTRAINT %I', c);
    END LOOP;
    ALTER TABLE webhook_events ADD CONSTRAINT webhook_events_processing_status_check
        CHECK (processing_status IN ('received','processed','ignored','ignored_unconfirmed','duplicate','dead_letter','failed','unmatched'));
END $$;
