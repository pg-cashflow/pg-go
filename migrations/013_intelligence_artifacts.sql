-- Migration 013: KPI/ROI snapshots, leakage, recommendations, forecasts, approvals.

CREATE TABLE IF NOT EXISTS kpi_snapshots (
  id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id          UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  period_month         CHAR(7) NOT NULL,
  snapshot_date        DATE NOT NULL,
  occupancy_bps        INT NOT NULL DEFAULT 0, -- occupancy * 10000 (82.00% = 8200)
  occupied_beds        INT NOT NULL DEFAULT 0,
  capacity_beds        INT NOT NULL DEFAULT 0,
  contribution_per_bed_paise BIGINT NOT NULL DEFAULT 0,
  opex_paise           BIGINT NOT NULL DEFAULT 0,
  ocf_paise            BIGINT NOT NULL DEFAULT 0,
  leakage_total_paise  BIGINT NOT NULL DEFAULT 0,
  variance_bridge_json JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_kpi_snapshot UNIQUE (property_id, snapshot_date)
);
CREATE INDEX IF NOT EXISTS idx_kpi_snapshots_period ON kpi_snapshots (property_id, period_month);

CREATE TABLE IF NOT EXISTS roi_snapshots (
  id                         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id                UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  period_month               CHAR(7) NOT NULL,
  snapshot_date              DATE NOT NULL,
  capital_invested_paise     BIGINT NOT NULL DEFAULT 0,
  capital_recovered_paise    BIGINT NOT NULL DEFAULT 0,
  unrecovered_paise          BIGINT NOT NULL DEFAULT 0,
  tbe_months_milli           INT, -- TBE * 1000; null if FCF <= 0
  break_even_occupancy_bps   INT,
  official                   BOOLEAN NOT NULL DEFAULT false, -- true only after tie-out closed
  created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_roi_snapshot UNIQUE (property_id, snapshot_date)
);
CREATE INDEX IF NOT EXISTS idx_roi_snapshots_period ON roi_snapshots (property_id, period_month);

CREATE TABLE IF NOT EXISTS leakage_events (
  id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id        UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  category           VARCHAR(40) NOT NULL,
  estimated_paise    BIGINT NOT NULL DEFAULT 0,
  confidence_bps     INT NOT NULL DEFAULT 10000,
  evidence_json      JSONB NOT NULL DEFAULT '{}'::jsonb,
  severity           SMALLINT NOT NULL DEFAULT 1, -- 1 monitor, 2 attention, 3 action
  status             VARCHAR(24) NOT NULL DEFAULT 'open',
  detected_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  created_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_leakage_property ON leakage_events (property_id, detected_at DESC);

CREATE TABLE IF NOT EXISTS recommendations (
  id                     UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id            UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  leakage_event_id       UUID REFERENCES leakage_events(id),
  issue                  TEXT NOT NULL,
  suggested_action       TEXT NOT NULL,
  expected_savings_paise BIGINT NOT NULL DEFAULT 0,
  effort                 VARCHAR(16) NOT NULL DEFAULT 'low',
  risk                   VARCHAR(16) NOT NULL DEFAULT 'low',
  confidence_bps         INT NOT NULL DEFAULT 8000,
  safety_ok              BOOLEAN NOT NULL DEFAULT true,
  quality_ok             BOOLEAN NOT NULL DEFAULT true,
  status                 VARCHAR(24) NOT NULL DEFAULT 'detected', -- detected | accepted | rejected | implemented | completed
  realized_savings_paise BIGINT,
  accepted_at            TIMESTAMPTZ,
  completed_at           TIMESTAMPTZ,
  created_at             TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_recommendations_property ON recommendations (property_id, created_at DESC);

CREATE TABLE IF NOT EXISTS forecast_snapshots (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id    UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  horizon_days   INT NOT NULL,
  as_of          DATE NOT NULL,
  payload_json   JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_forecast_snapshot UNIQUE (property_id, horizon_days, as_of)
);

CREATE TABLE IF NOT EXISTS approval_requests (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id    UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  kind           VARCHAR(32) NOT NULL, -- expense | reimbursement | budget
  subject_id     UUID NOT NULL,
  amount_paise   BIGINT NOT NULL,
  requested_by   UUID NOT NULL REFERENCES users(id),
  status         VARCHAR(24) NOT NULL DEFAULT 'pending', -- pending | approved | rejected
  decided_by     UUID REFERENCES users(id),
  decided_at     TIMESTAMPTZ,
  note           TEXT,
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_approval_requests_property ON approval_requests (property_id, status);

CREATE TABLE IF NOT EXISTS meal_prep_actuals (
  id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  property_id    UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
  meal_date      DATE NOT NULL,
  meal_slot      VARCHAR(16) NOT NULL, -- breakfast | lunch | dinner
  prepared_count INT NOT NULL,
  discarded_count INT NOT NULL DEFAULT 0,
  recorded_by    UUID REFERENCES users(id),
  created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  CONSTRAINT uq_meal_prep UNIQUE (property_id, meal_date, meal_slot)
);
