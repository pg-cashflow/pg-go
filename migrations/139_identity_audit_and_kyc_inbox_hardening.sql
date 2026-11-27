-- 139_identity_audit_and_kyc_inbox_hardening.sql
--
-- 1. Preserve immutable identity audit history under user deletion (F-07).
--    Migration 137 created user_identity_audit_events with:
--      - user_id REFERENCES users(id) ON DELETE CASCADE
--      - actor_user_id REFERENCES users(id) ON DELETE SET NULL
--    Those referential actions bypass table-level GRANT/RLS protections and can
--    delete or mutate supposedly append-only audit records when a user row is
--    deleted. Drop both FKs so user_id and actor_user_id remain immutable UUID
--    snapshot references.
--
-- 2. Partial index and explicit DELETE grant for bounded 30-day retention cleanup
--    of terminal ('processed' and 'duplicate') kyc_webhook_inbox rows (F-08).

SET lock_timeout = '5s';

ALTER TABLE user_identity_audit_events
    DROP CONSTRAINT IF EXISTS user_identity_audit_events_user_id_fkey;

ALTER TABLE user_identity_audit_events
    DROP CONSTRAINT IF EXISTS user_identity_audit_events_actor_user_id_fkey;

CREATE INDEX IF NOT EXISTS idx_kyc_webhook_inbox_processed_retention
    ON kyc_webhook_inbox (COALESCE(processed_at, received_at))
    WHERE processing_status IN ('processed', 'duplicate');

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_maint') THEN
        GRANT DELETE ON kyc_webhook_inbox TO pgapp_maint;
    END IF;
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_worker') THEN
        GRANT DELETE ON kyc_webhook_inbox TO pgapp_worker;
    END IF;
END $$;
