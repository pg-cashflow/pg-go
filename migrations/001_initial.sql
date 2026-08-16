-- Phase 1 Rev 6 initial schema. No cash_notes (D3).

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- Properties: one physical building per row.
CREATE TABLE properties (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  name         VARCHAR(100) NOT NULL,
  address      TEXT,
  owner_phone  VARCHAR(15) NOT NULL,
  upi_vpa      VARCHAR(100) NOT NULL,   -- server-side only; never in API responses
  owner_name   VARCHAR(100) NOT NULL,   -- shown on magic-link payment page
  owner_email  VARCHAR(255) NOT NULL,   -- recipient for alerts + financial digest
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Tenants: one row per paying guest.
-- phone: NULLABLE. NULL = cash-only tenant; no OTP, no self-service, no automated reminders.
--   Postgres UNIQUE allows multiple NULLs natively (NULL ≠ NULL).
-- notice_period_days: captured at onboarding from agreed terms.
-- notice_given_at: null until POST /owner/tenants/:id/notice sets it.
-- credit_balance_paise: carry-forward on overpayment; auto-applied at next due creation.
-- due_day CHECK 1–28: enforced at DB and API handler.
CREATE TABLE tenants (
  id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id          UUID NOT NULL REFERENCES properties(id),
  name                 VARCHAR(100) NOT NULL,
  phone                VARCHAR(15) UNIQUE,
  room_number          VARCHAR(20),
  aadhaar_last4        CHAR(4),
  rent_amount          INTEGER NOT NULL,
  due_day              SMALLINT NOT NULL CHECK (due_day BETWEEN 1 AND 28),
  notice_period_days   SMALLINT NOT NULL DEFAULT 30,
  notice_given_at      TIMESTAMPTZ,
  credit_balance_paise INTEGER NOT NULL DEFAULT 0,
  status               VARCHAR(20) NOT NULL DEFAULT 'active', -- active | vacated
  created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_tenants_property ON tenants(property_id, status);

-- Dues: one row per billing period (rent) or one-time (deposit).
CREATE TABLE dues (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  due_code         VARCHAR(8) NOT NULL UNIQUE,
  tenant_id        UUID NOT NULL REFERENCES tenants(id),
  property_id      UUID NOT NULL REFERENCES properties(id),
  kind             VARCHAR(20) NOT NULL DEFAULT 'rent',       -- rent | deposit
  amount           INTEGER NOT NULL,                          -- paise (current)
  original_amount  INTEGER NOT NULL,                          -- paise (immutable)
  period_start     DATE NOT NULL,
  period_end       DATE NOT NULL,
  due_date         DATE NOT NULL,
  status           VARCHAR(20) NOT NULL DEFAULT 'pending',
  paid_at          TIMESTAMPTZ,
  created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_dues_tenant_status   ON dues(tenant_id, status);
CREATE INDEX idx_dues_property_status ON dues(property_id, status);
CREATE INDEX idx_dues_due_date        ON dues(due_date) WHERE status IN ('pending', 'partial');
CREATE INDEX idx_dues_due_code        ON dues(due_code);
CREATE INDEX idx_dues_kind            ON dues(tenant_id, kind);

-- Users: completed-auth accounts (before payments so recorded_by FK resolves).
CREATE TABLE users (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  phone         VARCHAR(15) NOT NULL UNIQUE,
  role          VARCHAR(20) NOT NULL,
  tenant_id     UUID REFERENCES tenants(id),
  property_id   UUID REFERENCES properties(id),
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_login_at TIMESTAMPTZ
);

-- Payments: one or more matched transactions per due.
-- Partial UPI may produce several rows; upi_txn_id UNIQUE rejects duplicate bank txns.
CREATE TABLE payments (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  due_id       UUID NOT NULL REFERENCES dues(id),
  tenant_id    UUID NOT NULL REFERENCES tenants(id),
  upi_txn_id   VARCHAR(50) UNIQUE,
  amount       INTEGER NOT NULL,
  matched_by   VARCHAR(30) NOT NULL,
  recorded_by  UUID REFERENCES users(id),
  matched_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  raw_note     TEXT,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_payments_due ON payments(due_id);

-- Events: append-only fact ledger.
-- tenant_id nullable for property-scoped events (PaymentMatchFailed, FinancialSummarySent).
CREATE TABLE events (
  id           BIGSERIAL PRIMARY KEY,
  tenant_id    UUID REFERENCES tenants(id),
  property_id  UUID NOT NULL REFERENCES properties(id),
  event_type   VARCHAR(50) NOT NULL,
  due_id       UUID REFERENCES dues(id),
  occurred_at  TIMESTAMPTZ NOT NULL,
  payload      JSONB,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_events_tenant_type ON events(tenant_id, event_type) WHERE tenant_id IS NOT NULL;
CREATE INDEX idx_events_property    ON events(property_id, occurred_at DESC);

-- Magic-link tokens: intentionally stay valid after tenant vacates.
CREATE TABLE payment_tokens (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  due_id      UUID NOT NULL REFERENCES dues(id),
  token_hash  VARCHAR(64) NOT NULL UNIQUE,
  expires_at  TIMESTAMPTZ NOT NULL,
  used        BOOLEAN NOT NULL DEFAULT FALSE,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_tokens_due ON payment_tokens(due_id) WHERE used = FALSE;

-- Web Push subscriptions. Rows deleted by Vacate().
CREATE TABLE push_subscriptions (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id  UUID NOT NULL REFERENCES tenants(id),
  endpoint   TEXT NOT NULL UNIQUE,
  p256dh     TEXT NOT NULL,
  auth       TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- OTP store: short-lived, single-use. Plaintext never stored or logged.
CREATE TABLE otp_requests (
  id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  phone      VARCHAR(15) NOT NULL,
  otp_hash   VARCHAR(64) NOT NULL,
  attempts   SMALLINT NOT NULL DEFAULT 0,
  expires_at TIMESTAMPTZ NOT NULL,
  used       BOOLEAN NOT NULL DEFAULT FALSE,
  created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_otp_phone ON otp_requests(phone) WHERE used = FALSE;

-- Reminder log: idempotency guard + overdue escalation gate.
CREATE TABLE reminder_logs (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  due_id        UUID NOT NULL REFERENCES dues(id),
  reminder_type VARCHAR(10) NOT NULL,
  channel       VARCHAR(20) NOT NULL,
  sent_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_reminder_per_due_type_channel UNIQUE (due_id, reminder_type, channel)
);

-- CSV import log: import recency check for D+1/D+7 overdue escalation gate.
CREATE TABLE import_logs (
  id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id UUID NOT NULL REFERENCES properties(id),
  filename    TEXT NOT NULL,
  row_count   INTEGER NOT NULL,
  imported_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  imported_by UUID NOT NULL REFERENCES users(id)
);

-- Schema migrations tracking (used by cmd/migrate).
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    TEXT PRIMARY KEY,
  applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
