-- 137_user_identities.sql
-- Hybrid multi-identity mapping: separates canonical application users (users.id)
-- from external Firebase authentication identities (firebase_project_id, firebase_uid).
-- One Firebase UID maps to at most one application user; one user may have multiple
-- verified Firebase UIDs (e.g. legacy Phone UID + Google UID after explicit merge).

CREATE TABLE IF NOT EXISTS user_identities (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id               UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    firebase_project_id   TEXT NOT NULL DEFAULT '',
    firebase_uid          TEXT NOT NULL,
    provider              TEXT NOT NULL DEFAULT 'firebase',
    status                TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'revoked')),
    link_method           TEXT NOT NULL DEFAULT 'signup' CHECK (link_method IN ('signup', 'firebase_link', 'verified_merge', 'backfill')),
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    linked_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_authenticated_at TIMESTAMPTZ,
    CONSTRAINT uq_user_identities_project_uid UNIQUE (firebase_project_id, firebase_uid)
);

CREATE INDEX IF NOT EXISTS idx_user_identities_user_status
    ON user_identities (user_id, status);

CREATE INDEX IF NOT EXISTS idx_user_identities_uid_active
    ON user_identities (firebase_uid)
    WHERE status = 'active';

-- Backfill existing users.firebase_uid rows into user_identities.
INSERT INTO user_identities (
    user_id,
    firebase_project_id,
    firebase_uid,
    provider,
    status,
    link_method,
    created_at,
    linked_at,
    last_authenticated_at
)
SELECT
    id,
    '',
    firebase_uid,
    'firebase',
    'active',
    'backfill',
    created_at,
    created_at,
    last_login_at
FROM users
WHERE firebase_uid IS NOT NULL
  AND BTRIM(firebase_uid) <> ''
ON CONFLICT (firebase_project_id, firebase_uid) DO NOTHING;

-- Immutable audit trail for identity linking and verified account merges.
CREATE TABLE IF NOT EXISTS user_identity_audit_events (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    actor_user_id       UUID REFERENCES users(id) ON DELETE SET NULL,
    event_type          TEXT NOT NULL CHECK (event_type IN ('identity_linked', 'identity_merged', 'identity_revoked')),
    firebase_project_id TEXT NOT NULL DEFAULT '',
    firebase_uid        TEXT NOT NULL,
    source_user_id      UUID,
    metadata            JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_user_identity_audit_user_created
    ON user_identity_audit_events (user_id, created_at DESC);
