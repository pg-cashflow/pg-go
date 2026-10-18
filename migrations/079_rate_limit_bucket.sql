-- Migration 079: PostgreSQL-backed token bucket rate limiter
-- Provides durable, restart-safe, cross-instance rate limiting for sensitive endpoints.

CREATE TABLE IF NOT EXISTS rate_limit_bucket (
    bucket_key   TEXT PRIMARY KEY,
    tokens       DOUBLE PRECISION NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_rate_limit_bucket_updated_at
    ON rate_limit_bucket(updated_at);
