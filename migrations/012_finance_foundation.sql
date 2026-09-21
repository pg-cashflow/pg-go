-- Migration 012: ROI finance foundation (capital, expenses, journal, policies).
-- All money columns are bigint paise. Cost/OPEX is greenfield (no prior expense tables).

CREATE TABLE IF NOT EXISTS financial_categories (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id     UUID REFERENCES properties(id) ON DELETE CASCADE,
  code            VARCHAR(64) NOT NULL,
  name            VARCHAR(120) NOT NULL,
  cost_behavior   VARCHAR(24) NOT NULL DEFAULT 'variable', -- fixed | variable | semi_variable
  controllability VARCHAR(32) NOT NULL DEFAULT 'controllable', -- controllable | partial | uncontrollable
  purpose         VARCHAR(40) NOT NULL DEFAULT 'operations',
  is_system       BOOLEAN NOT NULL DEFAULT false,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_financial_categories_property_code
  ON financial_categories (property_id, code) WHERE property_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_financial_categories_system_code
  ON financial_categories (code) WHERE property_id IS NULL;

INSERT INTO financial_categories (property_id, code, name, cost_behavior, controllability, purpose, is_system)
VALUES
  (NULL, 'lease', 'Property lease', 'fixed', 'uncontrollable', 'operations', true),
  (NULL, 'salary', 'Salaries', 'fixed', 'partial', 'operations', true),
  (NULL, 'internet', 'Internet', 'fixed', 'partial', 'operations', true),
  (NULL, 'food', 'Food', 'variable', 'controllable', 'operations', true),
  (NULL, 'electricity', 'Electricity', 'variable', 'partial', 'operations', true),
  (NULL, 'water', 'Water', 'variable', 'partial', 'operations', true),
  (NULL, 'cleaning', 'Cleaning supplies', 'variable', 'controllable', 'operations', true),
  (NULL, 'maintenance', 'Maintenance', 'semi_variable', 'controllable', 'maintenance', true),
  (NULL, 'repairs', 'Repairs', 'variable', 'controllable', 'maintenance', true),
  (NULL, 'vendor', 'Vendor services', 'semi_variable', 'controllable', 'operations', true),
  (NULL, 'marketing', 'Marketing', 'variable', 'controllable', 'growth', true),
  (NULL, 'payment_processing', 'Payment processing (TDR)', 'variable', 'uncontrollable', 'operations', true),
  (NULL, 'loyalty_reward', 'Loyalty / reward liability', 'variable', 'controllable', 'customer_experience', true)
ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS property_finance_settings (
  property_id                    UUID PRIMARY KEY REFERENCES properties(id) ON DELETE CASCADE,
  fiscal_month_start_day         SMALLINT NOT NULL DEFAULT 1,
  manager_can_view_capital       BOOLEAN NOT NULL DEFAULT false,
  manager_can_view_roi           BOOLEAN NOT NULL DEFAULT false,
  manager_can_view_leakage       BOOLEAN NOT NULL DEFAULT true,
  tdr_effective_bps              INT NOT NULL DEFAULT 0, -- estimated TDR in basis points of Cashfree GMV
  tdr_is_estimated               BOOLEAN NOT NULL DEFAULT true,
  created_at                     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at                     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS approval_policies (
  property_id                    UUID PRIMARY KEY REFERENCES properties(id) ON DELETE CASCADE,
  manager_daily_limit_paise      BIGINT NOT NULL DEFAULT 1000000,
  single_expense_limit_paise     BIGINT NOT NULL DEFAULT 500000,
  manager_monthly_limit_paise    BIGINT NOT NULL DEFAULT 5000000,
  owner_approval_threshold_paise BIGINT NOT NULL DEFAULT 500000,
  reimbursement_threshold_paise  BIGINT NOT NULL DEFAULT 250000,
  emergency_bypass_enabled       BOOLEAN NOT NULL DEFAULT true,
  updated_at                     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS capital_transactions (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id      UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  owner_user_id    UUID NOT NULL REFERENCES users(id),
  kind             VARCHAR(24) NOT NULL, -- initial | additional | withdrawal
  amount_paise     BIGINT NOT NULL,
  purpose          TEXT,
  reference        VARCHAR(32) NOT NULL,
  idempotency_key  VARCHAR(128) NOT NULL,
  occurred_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT chk_capital_amount_positive CHECK (amount_paise > 0),
  CONSTRAINT uq_capital_idempotency UNIQUE (property_id, idempotency_key),
  CONSTRAINT uq_capital_reference UNIQUE (property_id, reference)
);
CREATE INDEX IF NOT EXISTS idx_capital_property_occurred ON capital_transactions (property_id, occurred_at);

CREATE TABLE IF NOT EXISTS expenses (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id      UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  category_code    VARCHAR(64) NOT NULL,
  vendor_name      VARCHAR(160),
  description      TEXT,
  amount_paise     BIGINT NOT NULL,
  status           VARCHAR(32) NOT NULL DEFAULT 'draft', -- draft | pending_approval | approved | paid | cancelled
  emergency        BOOLEAN NOT NULL DEFAULT false,
  room_id          UUID REFERENCES rooms(id),
  created_by       UUID NOT NULL REFERENCES users(id),
  created_by_role  VARCHAR(16) NOT NULL,
  idempotency_key  VARCHAR(128) NOT NULL,
  occurred_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT chk_expense_amount_positive CHECK (amount_paise > 0),
  CONSTRAINT uq_expense_idempotency UNIQUE (property_id, idempotency_key)
);
CREATE INDEX IF NOT EXISTS idx_expenses_property_occurred ON expenses (property_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_expenses_status ON expenses (property_id, status);

CREATE TABLE IF NOT EXISTS expense_payments (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  expense_id       UUID NOT NULL REFERENCES expenses(id) ON DELETE CASCADE,
  property_id      UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  amount_paise     BIGINT NOT NULL,
  payer_role       VARCHAR(16) NOT NULL, -- owner | manager
  payer_user_id    UUID NOT NULL REFERENCES users(id),
  method           VARCHAR(16) NOT NULL DEFAULT 'cash', -- cash | upi | bank
  idempotency_key  VARCHAR(128) NOT NULL,
  occurred_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT chk_expense_payment_positive CHECK (amount_paise > 0),
  CONSTRAINT uq_expense_payment_idempotency UNIQUE (property_id, idempotency_key)
);

CREATE TABLE IF NOT EXISTS manager_advances (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id      UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  manager_user_id  UUID NOT NULL REFERENCES users(id),
  expense_payment_id UUID NOT NULL REFERENCES expense_payments(id),
  amount_paise     BIGINT NOT NULL,
  idempotency_key  VARCHAR(128) NOT NULL,
  occurred_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_manager_advance_idempotency UNIQUE (property_id, idempotency_key)
);

CREATE TABLE IF NOT EXISTS manager_reimbursements (
  id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id      UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  manager_user_id  UUID NOT NULL REFERENCES users(id),
  amount_paise     BIGINT NOT NULL,
  recorded_by      UUID NOT NULL REFERENCES users(id),
  idempotency_key  VARCHAR(128) NOT NULL,
  occurred_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT chk_reimburse_positive CHECK (amount_paise > 0),
  CONSTRAINT uq_reimburse_idempotency UNIQUE (property_id, idempotency_key)
);

CREATE TABLE IF NOT EXISTS financial_journal_entries (
  id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id   UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  account_code  VARCHAR(64) NOT NULL,
  debit_paise   BIGINT NOT NULL DEFAULT 0,
  credit_paise  BIGINT NOT NULL DEFAULT 0,
  source_type   VARCHAR(40) NOT NULL,
  source_id     UUID NOT NULL,
  line_kind     VARCHAR(40) NOT NULL,
  occurred_at   TIMESTAMPTZ NOT NULL,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT chk_journal_one_side CHECK (
    (debit_paise > 0 AND credit_paise = 0) OR (credit_paise > 0 AND debit_paise = 0)
  ),
  CONSTRAINT uq_journal_source_line UNIQUE (source_type, source_id, line_kind)
);
CREATE INDEX IF NOT EXISTS idx_journal_property_occurred ON financial_journal_entries (property_id, occurred_at);
CREATE INDEX IF NOT EXISTS idx_journal_account ON financial_journal_entries (property_id, account_code, occurred_at);

CREATE TABLE IF NOT EXISTS budgets (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id    UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  category_code  VARCHAR(64) NOT NULL,
  period_month   CHAR(7) NOT NULL, -- YYYY-MM
  amount_paise   BIGINT NOT NULL,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_budget_period UNIQUE (property_id, category_code, period_month)
);

CREATE TABLE IF NOT EXISTS reward_liability_transactions (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id    UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  tenant_id      UUID REFERENCES tenants(id),
  kind           VARCHAR(24) NOT NULL, -- issued | redeemed | expired_breakage | adjusted
  points         INT NOT NULL DEFAULT 0,
  amount_paise   BIGINT NOT NULL,
  source_type    VARCHAR(40) NOT NULL,
  source_id      UUID NOT NULL,
  occurred_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_reward_liability_source UNIQUE (source_type, source_id, kind)
);
CREATE INDEX IF NOT EXISTS idx_reward_liability_property ON reward_liability_transactions (property_id, occurred_at);

CREATE TABLE IF NOT EXISTS period_tie_outs (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id       UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  period_month      CHAR(7) NOT NULL,
  recon_total_paise BIGINT NOT NULL DEFAULT 0,
  ledger_total_paise BIGINT NOT NULL DEFAULT 0,
  difference_paise  BIGINT NOT NULL DEFAULT 0,
  bridge_json       JSONB NOT NULL DEFAULT '[]'::jsonb,
  status            VARCHAR(16) NOT NULL DEFAULT 'open', -- open | closed
  closed_at         TIMESTAMPTZ,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_period_tie_out UNIQUE (property_id, period_month)
);

CREATE TABLE IF NOT EXISTS expense_import_suggestions (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id    UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  txn_id         VARCHAR(128) NOT NULL,
  amount_paise   BIGINT NOT NULL,
  txn_date       DATE NOT NULL,
  note           TEXT,
  status         VARCHAR(24) NOT NULL DEFAULT 'pending', -- pending | accepted | dismissed
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_expense_import_txn UNIQUE (property_id, txn_id)
);

INSERT INTO property_finance_settings (property_id)
SELECT id FROM properties
ON CONFLICT (property_id) DO NOTHING;

INSERT INTO approval_policies (property_id)
SELECT id FROM properties
ON CONFLICT (property_id) DO NOTHING;
