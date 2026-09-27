# Ticket 12: Track M — Staff Attendance & Wage-Calculation Engine

- **Type**: `wayfinder:task`
- **Status**: Resolved
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Prerequisite**: Track L (Cashfree Transfers V2 & Payout Batch Infrastructure) verified and committed.

---

## 1. Objective

Design and implement a clean, isolated `internal/attendance` domain and wage calculation module that computes monthly net wage disbursements based on per-property leave policies and daily attendance records, feeding integer-paise amounts into the existing `payout_items` (`PayeeTypeStaff`) pipeline without touching or expanding the blast radius of the audited payout rails.

---

## 2. Architecture & Domain Separation

Following the modular monolith conventions and ADR separation of concerns:
- **Strict Isolation**: `internal/attendance` is strictly upstream of the payout subsystem (`internal/postgres/payout_repo.go`, `internal/cashfree/`, `internal/finance/payout_dispatcher.go`).
- **No Payout Core Modifications**: Payout core tables (`payout_batches`, `payout_items`, `payout_payees`), maker-checker approval, cryptographic step-up OTP, and Transfers V2 client remain untouched.
- **Contract Interface**: The wage calculation module outputs a standard `domain.PayoutItem` with:
  - `PayeeType`: `domain.PayeeTypeStaff`
  - `PayeeID`: `staff.PayeeID` (foreign key to `payout_payees.id`)
  - `AmountPaise`: `calculated.NetWagePaise`
  - `Purpose`: `"Salary <Month YYYY> - Days: <P>/<Total>"`
  - `ReferenceNumber`: `"SAL-<YYYYMM>-<StaffShortID>"`

---

## 3. Database Schema (Migration 025: `025_staff_attendance_and_wages.sql`)

### A. `staff_profiles`
Associates staff members with a property, their base compensation, and their designated `payout_payees` bank/UPI record.
```sql
CREATE TABLE staff_profiles (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id) ON DELETE CASCADE,
    payee_id UUID NOT NULL REFERENCES payout_payees(id) ON DELETE RESTRICT,
    name TEXT NOT NULL,
    role TEXT NOT NULL, -- Free-text (e.g. 'cook', 'warden', 'security', 'gardener', 'electrician'); owner-managed without hardcoded enum constraints
    phone VARCHAR(15),
    base_monthly_wage_paise BIGINT NOT NULL CHECK (base_monthly_wage_paise > 0),
    effective_from DATE NOT NULL,
    effective_to DATE,
    status VARCHAR(20) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'inactive')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX uq_staff_active_payee 
    ON staff_profiles (property_id, payee_id) 
    WHERE status = 'active';

CREATE INDEX idx_staff_property_status 
    ON staff_profiles (property_id, status);
```

### B. `leave_policies`
Per-property leave rules. Allows each property owner to configure monthly free leaves and public holidays.
```sql
CREATE TABLE leave_policies (
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
```

### C. `attendance_records`
Daily attendance tracking per staff member.
```sql
CREATE TABLE attendance_records (
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

CREATE INDEX idx_attendance_property_date 
    ON attendance_records (property_id, work_date);
CREATE INDEX idx_attendance_staff_month 
    ON attendance_records (staff_id, work_date);
```

### D. `wage_calculations`
Immutable audit snapshot of monthly wage calculations prior to batching.
```sql
CREATE TABLE wage_calculations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    property_id UUID NOT NULL REFERENCES properties(id),
    staff_id UUID NOT NULL REFERENCES staff_profiles(id),
    cycle_month CHAR(7) NOT NULL, -- 'YYYY-MM'
    base_monthly_wage_paise BIGINT NOT NULL,
    prorated_base_wage_paise BIGINT NOT NULL, -- Equals base_monthly_wage_paise unless hired or departed mid-cycle
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
    payout_item_id UUID REFERENCES payout_items(id),
    status VARCHAR(20) NOT NULL DEFAULT 'calculated' CHECK (status IN ('calculated', 'batched', 'cancelled')),
    calculated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finalized_by UUID NOT NULL REFERENCES users(id),
    CONSTRAINT uq_staff_wage_cycle UNIQUE (property_id, staff_id, cycle_month)
);
```

---

## 4. Wage Calculation Invariants & Precision Math

1. **Zero Floats Rule**: All currency calculations are conducted in integer paise (`int64`).
2. **Total Basis Days ($D_{\text{basis}}$)**:
   - For `calendar_days`: Total days in month (e.g. 28, 29, 30, 31).
   - For `fixed_30`: Always 30 days.
   - For `working_days_excluding_sundays`: Calendar days minus count of Sundays in month.
3. **Mid-Cycle Hire / Departure Proration**:
   - If a staff member joins mid-month (`effective_from > cycle_start`) or leaves mid-month (`effective_to != nil && effective_to < cycle_end`):
     - Active employed window:
       $$[\text{window\_start}, \text{window\_end}] = [\max(\text{cycle\_start}, \text{effective\_from}), \min(\text{cycle\_end}, \text{effective\_to})]$$
     - If $\text{window\_end} < \text{window\_start}$, employed days = 0, Net Wage = 0.
     - Employed basis days $D_{\text{employed}}$: Number of basis days falling within the active employed window.
     - Prorated Base Monthly Wage:
       $$\text{ProratedBasePaise} = \left\lfloor \text{BaseMonthlyWagePaise} \times \frac{D_{\text{employed}}}{D_{\text{basis}}} \right\rfloor$$
     - Attendance records (present, absent, leave) are recorded and evaluated only within $[\text{window\_start}, \text{window\_end}]$.
     - Pro-rated Free Leave Days Allowed:
       $$\text{FreeLeaveDaysAllowed} = \frac{\text{MonthlyFreeLeaveDays} \times D_{\text{employed}}}{D_{\text{basis}}}$$
4. **Per-Day Rate ($R_{\text{day}}$)**:
   $$R_{\text{day}} = \left\lfloor \frac{\text{BaseMonthlyWagePaise}}{D_{\text{basis}}} \right\rfloor$$
5. **Attendance Accounting & Missing Data Visibility**:
   - $\text{Present} = \text{Count}(\text{present}) + 0.5 \times \text{Count}(\text{half\_day})$
   - $\text{Absent} = \text{Count}(\text{absent}) + 0.5 \times \text{Count}(\text{half\_day})$
   - $\text{PaidLeave} = \text{Count}(\text{paid\_leave})$
   - $\text{Holidays} = \text{Count}(\text{holiday})$
   - **Unrecorded Days Visibility**: If an owner or manager forgets to log attendance on a working day, it defaults to absent but is explicitly tracked and surfaced in $\text{DaysUnrecorded}$. This field is saved in `wage_calculations.days_unrecorded` and displayed in the `/payroll/calculate` preview so owners see missing check-ins before finalizing.
6. **Excess Absent Days**:
   $$\text{ExcessAbsentDays} = \max\left(0, \text{Absent} - \text{FreeLeaveDaysAllowed}\right)$$
7. **Total Deduction (Pure Integer Half-Day Arithmetic)**:
   To eliminate any floating-point arithmetic in currency calculations, excess absences are scaled to integer half-days (2 units per day):
   $$\text{ExcessHalfDays} = \text{int64}\left(\text{round}\left(\text{ExcessAbsentDays} \times 2\right)\right)$$
   $$\text{TotalDeductionPaise} = \frac{\text{ExcessHalfDays} \times R_{\text{day}}}{2}$$
   *(100% pure integer paise math; zero floating-point operations in monetary computation).*
8. **Net Wage (Floor Guard & Clamping)**:
   $$\text{NetWagePaise} = \max\left(0, \text{ProratedBasePaise} - \text{TotalDeductionPaise}\right)$$
   *(Net wage can never be negative; excess absences cannot create a debt to the employer).*

---

## 5. API Endpoints

All endpoints gated by `PropertyID` tenancy check and owner authentication (`UserRoleOwner` or `UserRoleManager`):
- `POST /api/owner/attendance/daily`: Bulk mark/update attendance for property staff for a given date.
- `GET /api/owner/attendance/monthly`: Get full attendance grid for staff in a given month.
- `GET /api/owner/attendance/leave-policy`: Get current property leave policy (or default if unset).
- `PUT /api/owner/attendance/leave-policy`: Update property leave policy (free days, holidays).
- `POST /api/owner/payroll/calculate`: Run wage calculation preview for all staff for `YYYY-MM`.
- `POST /api/owner/payroll/finalize`: Finalize calculations, write `wage_calculations`, and insert unbatched `payout_items` (`PayeeTypeStaff`).

---

## 6. Implementation Steps

1. **Step 1: Database Migration**: Create `migrations/025_staff_attendance_and_wages.sql`.
2. **Step 2: Domain Types & Invariant Engine**:
   - `internal/domain/attendance.go` defining structs, enums, and calculation contracts.
   - `internal/attendance/calculator.go` pure calculation engine without DB dependencies.
   - `internal/attendance/calculator_test.go` property and fuzz tests covering all month lengths, leap years, half-day combinations, and deduction bounds.
3. **Step 3: Repository & DBTX Persistence**:
   - `internal/postgres/attendance_repo.go` implementing `StaffRepo`, `AttendanceRepo`, `LeavePolicyRepo`, `WageCalculationRepo` using dual-mode `DBTX`.
4. **Step 4: Bridge to Payout Items**:
   - `internal/attendance/service.go` containing `FinalizePayrollCycleTx` which atomically saves `wage_calculations` and enqueues unbatched `payout_items`.
5. **Step 5: API Handlers & Router Integration**:
   - `internal/api/handlers_attendance.go` for daily check-in, policy management, and calculation/finalization endpoints.
   - Wire dependencies into `internal/api/router.go`.
6. **Step 6: Live Integration Tests**:
   - `internal/postgres/attendance_repo_live_test.go` and `internal/api/handlers_attendance_test.go`.

---

## 7. Verification Criteria

- [x] Zero floats in wage calculation; exact integer paise arithmetic.
- [x] Payout core packages remain completely untouched.
- [x] Invariant holds: $\text{NetWagePaise} + \text{TotalDeductionPaise} \le \text{BaseMonthlyWagePaise}$.
- [x] Finalized wage items generate valid unbatched `payout_items` that seamlessly feed into `CreateBatchFromUnbatchedItems` and maker-checker approval.
- [x] 100% test pass across `go test -p 2 ./internal/...`.

---

## 8. Implementation Proof & Exact File References

1. **Migration 025**: [migrations/025_staff_attendance_and_wages.sql](file:///c:/Users/divak/Downloads/pg-go/migrations/025_staff_attendance_and_wages.sql) (`staff_profiles`, `leave_policies`, `attendance_records`, `wage_calculations`).
2. **Domain Models**: [internal/domain/attendance.go](file:///c:/Users/divak/Downloads/pg-go/internal/domain/attendance.go).
3. **Calculation Engine**: [internal/attendance/calculator.go](file:///c:/Users/divak/Downloads/pg-go/internal/attendance/calculator.go), tested in [internal/attendance/calculator_test.go](file:///c:/Users/divak/Downloads/pg-go/internal/attendance/calculator_test.go).
4. **Repository**: [internal/postgres/attendance_repo.go](file:///c:/Users/divak/Downloads/pg-go/internal/postgres/attendance_repo.go), tested with live PostgreSQL in [internal/postgres/attendance_repo_live_test.go](file:///c:/Users/divak/Downloads/pg-go/internal/postgres/attendance_repo_live_test.go).
5. **Payout Bridge & Service**: [internal/attendance/service.go](file:///c:/Users/divak/Downloads/pg-go/internal/attendance/service.go), with `CreatePayoutItemTx` in [internal/postgres/payout_repo.go](file:///c:/Users/divak/Downloads/pg-go/internal/postgres/payout_repo.go).
6. **API Handlers & Router**: [internal/api/handlers_attendance.go](file:///c:/Users/divak/Downloads/pg-go/internal/api/handlers_attendance.go), wired in [internal/api/router.go](file:///c:/Users/divak/Downloads/pg-go/internal/api/router.go) and [cmd/server/main.go](file:///c:/Users/divak/Downloads/pg-go/cmd/server/main.go), tested end-to-end in [internal/api/handlers_attendance_test.go](file:///c:/Users/divak/Downloads/pg-go/internal/api/handlers_attendance_test.go).
7. **Test Suite Status**: All 31 packages in `internal/...` pass 100% green with `go test -p 2 ./internal/...` and `go test -count=1`.
