-- Migration 025: Staff Profiles, Leave Policy, Daily Attendance and Monthly Wage Calculation Engine
-- Upstream domain module for staff payroll calculation feeding into payout_items (PayeeTypeStaff)

-- 1. Staff Profiles: Links a staff member to a property, their payout payee (bank/UPI), and base monthly compensation
CREATE TABLE IF NOT EXISTS staff_profiles (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    payee_id UUID NOT NULL REFERENCES payout_payees(id) ON DELETE RESTRICT,
    name TEXT NOT NULL,
    role TEXT NOT NULL, -- Free-text (e.g. 'cook', 'warden', 'security', 'housekeeping', 'gardener', 'electrician')
    phone VARCHAR(15),
    base_monthly_wage_paise BIGINT NOT NULL CHECK (base_monthly_wage_paise > 0),
    effective_from DATE NOT NULL,
    effective_to DATE,
    status VARCHAR(20) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT chk_staff_effective_window CHECK (effective_to IS NULL OR effective_to >= effective_from)
);

CREATE UNIQUE INDEX IF NOT EXISTS uq_staff_active_payee 
    ON staff_profiles (property_id, payee_id) 
    WHERE status = 'active';

CREATE INDEX IF NOT EXISTS idx_staff_property_status 
    ON staff_profiles (property_id, status);

-- 2. Leave Policies: Per-property leave rules, free leave allowances, and working day calculation basis
CREATE TABLE IF NOT EXISTS leave_policies (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    monthly_free_leave_days SMALLINT NOT NULL DEFAULT 2 CHECK (monthly_free_leave_days >= 0),
    paid_holidays JSONB NOT NULL DEFAULT '[]'::jsonb, -- e.g. ["2026-01-26", "2026-08-15", "2026-10-02"]
    working_days_basis VARCHAR(30) NOT NULL DEFAULT 'calendar_days' 
        CHECK (working_days_basis IN ('calendar_days', 'fixed_30', 'working_days_excluding_sundays')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_property_leave_policy UNIQUE (property_id)
);

-- 3. Daily Attendance Records: Per-staff daily check-ins
CREATE TABLE IF NOT EXISTS attendance_records (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    staff_id UUID NOT NULL REFERENCES staff_profiles(id) ON DELETE CASCADE,
    work_date DATE NOT NULL,
    status VARCHAR(20) NOT NULL CHECK (status IN ('present', 'absent', 'paid_leave', 'half_day', 'holiday')),
    notes TEXT,
    recorded_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT uq_staff_attendance_date UNIQUE (property_id, staff_id, work_date)
);

CREATE INDEX IF NOT EXISTS idx_attendance_property_date 
    ON attendance_records (property_id, work_date);
CREATE INDEX IF NOT EXISTS idx_attendance_staff_month 
    ON attendance_records (staff_id, work_date);

-- 4. Wage Calculations: Immutable monthly cycle audit snapshot prior to batching
CREATE TABLE IF NOT EXISTS wage_calculations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    staff_id UUID NOT NULL REFERENCES staff_profiles(id) ON DELETE CASCADE,
    cycle_month CHAR(7) NOT NULL, -- 'YYYY-MM'
    base_monthly_wage_paise BIGINT NOT NULL,
    prorated_base_wage_paise BIGINT NOT NULL,
    total_basis_days INT NOT NULL,
    employed_basis_days INT NOT NULL,
    days_present NUMERIC(4, 1) NOT NULL,
    days_paid_leave NUMERIC(4, 1) NOT NULL,
    days_holiday NUMERIC(4, 1) NOT NULL,
    days_absent NUMERIC(4, 1) NOT NULL,
    days_unrecorded NUMERIC(4, 1) NOT NULL DEFAULT 0,
    free_leave_days_allowed NUMERIC(4, 1) NOT NULL,
    excess_absent_days NUMERIC(4, 1) NOT NULL,
    per_day_rate_paise BIGINT NOT NULL,
    total_deduction_paise BIGINT NOT NULL,
    net_wage_paise BIGINT NOT NULL,
    payout_item_id UUID REFERENCES payout_items(id) ON DELETE SET NULL,
    status VARCHAR(20) NOT NULL DEFAULT 'calculated' CHECK (status IN ('calculated', 'batched', 'cancelled')),
    calculated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finalized_by UUID NOT NULL REFERENCES users(id),
    CONSTRAINT uq_staff_wage_cycle UNIQUE (property_id, staff_id, cycle_month)
);

CREATE INDEX IF NOT EXISTS idx_wage_cycle_property_month 
    ON wage_calculations (property_id, cycle_month, status);

