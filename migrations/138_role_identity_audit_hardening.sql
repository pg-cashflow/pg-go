-- 138_role_identity_audit_hardening.sql
--
-- Two independent changes:
--
-- 1. Role inheritance hardening.
--    pgapp_maint and pgapp_worker hold membership in pgapp_app so they
--    can SET LOCAL ROLE pgapp_app inside a transaction. Prior to this migration
--    the membership was INHERIT (the default), which also granted the full set
--    of pgapp_app table privileges to any connection running as the maintenance
--    or worker role — without an explicit SET ROLE. This migration changes the
--    membership to INHERIT FALSE, SET TRUE. The maintenance and worker roles
--    retain the ability to switch into pgapp_app via SET [LOCAL] ROLE, but they
--    no longer inherit pgapp_app privileges passively.
--
--    PostgreSQL 16+ syntax: GRANT role TO grantee WITH INHERIT FALSE, SET TRUE.
--    The SET clause controls whether SET ROLE is permitted (it must remain TRUE
--    for existing scoped-transaction patterns). The INHERIT clause controls
--    passive inheritance (we set it to FALSE).
--
--    If pgapp_maint or pgapp_worker are not members of pgapp_app this
--    statement is a no-op for that role; it will not error.
--
-- 2. Append-only audit protection for user_identity_audit_events.
--    Authentication identity changes must leave an immutable trail. This
--    migration prevents DELETE and UPDATE on the audit table for the application
--    role (pgapp_app). The maintenance role may still run archival queries via
--    SET LOCAL ROLE pgapp_app within a tightly-scoped transaction; a separate
--    archival policy (not automated deletion) is the correct path for retention.
--
-- Safety: this migration is non-destructive. It narrows privileges but does not
-- drop any object, column, or row. Roll-back is GRANT ... WITH INHERIT TRUE
-- (restoring the former behaviour) if regression tests reveal unexpected issues.

-- ── 1. NOINHERIT membership for maintenance and worker roles ─────────────────

SET lock_timeout = '5s';

DO $$
BEGIN
    -- Re-grant the pgapp_app membership to pgapp_maint with INHERIT FALSE.
    -- The REVOKE+GRANT pattern is used because ALTER ROLE ... WITH INHERIT for a
    -- specific role membership requires the GRANT ... WITH INHERIT FALSE syntax
    -- available from PG16; we use DO block for forward-compat defensive coding.
    IF EXISTS (
        SELECT 1 FROM pg_auth_members m
        JOIN pg_roles r  ON r.oid  = m.roleid
        JOIN pg_roles g  ON g.oid  = m.member
        WHERE r.rolname = 'pgapp_app' AND g.rolname = 'pgapp_maint'
    ) THEN
        REVOKE pgapp_app FROM pgapp_maint;
    END IF;
END $$;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_maint')
       AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_app') THEN
        -- INHERIT FALSE: pgapp_maint does not passively inherit pgapp_app privs.
        -- SET TRUE: pgapp_maint may still SET [LOCAL] ROLE pgapp_app.
        EXECUTE 'GRANT pgapp_app TO pgapp_maint WITH INHERIT FALSE, SET TRUE';
    END IF;
END $$;

DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_auth_members m
        JOIN pg_roles r  ON r.oid  = m.roleid
        JOIN pg_roles g  ON g.oid  = m.member
        WHERE r.rolname = 'pgapp_app' AND g.rolname = 'pgapp_worker'
    ) THEN
        REVOKE pgapp_app FROM pgapp_worker;
    END IF;
END $$;

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_worker')
       AND EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_app') THEN
        EXECUTE 'GRANT pgapp_app TO pgapp_worker WITH INHERIT FALSE, SET TRUE';
    END IF;
END $$;

-- ── 2. Append-only protection on the identity audit table ────────────────────

-- Revoke DELETE and UPDATE from the application role.
-- INSERT and SELECT remain to allow audit event writes and reads.
-- pgapp_maint retains DELETE via its own grants (for archival workflows).
REVOKE DELETE, UPDATE ON user_identity_audit_events FROM pgapp_app;

-- Belt-and-braces: a row-security policy that blocks UPDATE and DELETE from
-- the application role at the row level, independent of GRANT-level permissions.
-- This survives a future accidental GRANT of DELETE back to pgapp_app.
ALTER TABLE user_identity_audit_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_identity_audit_events FORCE ROW LEVEL SECURITY;

-- Allow the application role to INSERT new rows (append-only).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE tablename = 'user_identity_audit_events'
          AND policyname = 'audit_app_insert_only'
    ) THEN
        EXECUTE $pol$
            CREATE POLICY audit_app_insert_only
            ON user_identity_audit_events
            FOR INSERT
            TO pgapp_app
            WITH CHECK (true)
        $pol$;
    END IF;
END $$;

-- Allow the application role to SELECT its own audit rows.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_policies
        WHERE tablename = 'user_identity_audit_events'
          AND policyname = 'audit_app_select'
    ) THEN
        EXECUTE $pol$
            CREATE POLICY audit_app_select
            ON user_identity_audit_events
            FOR SELECT
            TO pgapp_app
            USING (true)
        $pol$;
    END IF;
END $$;

-- Maintenance role may SELECT for archival/review without an explicit SET ROLE.
-- Note: maintenance accesses via SET LOCAL ROLE pgapp_app will use the app
-- policies above. The following policy covers direct maintenance-role access.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pgapp_maint') THEN
        IF NOT EXISTS (
            SELECT 1 FROM pg_policies
            WHERE tablename = 'user_identity_audit_events'
              AND policyname = 'audit_maintenance_select'
        ) THEN
            EXECUTE $pol$
                CREATE POLICY audit_maintenance_select
                ON user_identity_audit_events
                FOR SELECT
                TO pgapp_maint
                USING (true)
            $pol$;
        END IF;
    END IF;
END $$;
