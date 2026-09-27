package domain

import (
	"time"

	"github.com/google/uuid"
)

// StaffProfileStatus represents active or inactive status of a staff member.
type StaffProfileStatus string

const (
	StaffActive   StaffProfileStatus = "active"
	StaffInactive StaffProfileStatus = "inactive"
)

// StaffProfile links a staff member to their property, base compensation, and payout payee.
type StaffProfile struct {
	ID                  uuid.UUID          `json:"id"`
	PropertyID          uuid.UUID          `json:"property_id"`
	PayeeID             uuid.UUID          `json:"payee_id"`
	Name                string             `json:"name"`
	Role                string             `json:"role"`
	Phone               *string            `json:"phone,omitempty"`
	BaseMonthlyWagePaise int64             `json:"base_monthly_wage_paise"`
	EffectiveFrom       time.Time          `json:"effective_from"`
	EffectiveTo         *time.Time         `json:"effective_to,omitempty"`
	Status              StaffProfileStatus `json:"status"`
	CreatedAt           time.Time          `json:"created_at"`
	UpdatedAt           time.Time          `json:"updated_at"`
}

// WorkingDaysBasis defines how total basis days in a month are computed.
type WorkingDaysBasis string

const (
	WorkingDaysBasisCalendarDays    WorkingDaysBasis = "calendar_days"
	WorkingDaysBasisFixed30         WorkingDaysBasis = "fixed_30"
	WorkingDaysBasisExcludingSundays WorkingDaysBasis = "working_days_excluding_sundays"
)

// LeavePolicy configures property-specific attendance allowances and holiday calendars.
type LeavePolicy struct {
	ID                   uuid.UUID        `json:"id"`
	PropertyID           uuid.UUID        `json:"property_id"`
	MonthlyFreeLeaveDays int              `json:"monthly_free_leave_days"`
	PaidHolidays         []string         `json:"paid_holidays"` // Dates in "YYYY-MM-DD" format
	WorkingDaysBasis     WorkingDaysBasis `json:"working_days_basis"`
	CreatedAt            time.Time        `json:"created_at"`
	UpdatedAt            time.Time        `json:"updated_at"`
}

// AttendanceStatus represents the daily check-in outcome.
type AttendanceStatus string

const (
	AttendancePresent   AttendanceStatus = "present"
	AttendanceAbsent    AttendanceStatus = "absent"
	AttendancePaidLeave AttendanceStatus = "paid_leave"
	AttendanceHalfDay   AttendanceStatus = "half_day"
	AttendanceHoliday   AttendanceStatus = "holiday"
)

// AttendanceRecord tracks daily attendance for a single staff member on a specific date.
type AttendanceRecord struct {
	ID         uuid.UUID        `json:"id"`
	PropertyID uuid.UUID        `json:"property_id"`
	StaffID    uuid.UUID        `json:"staff_id"`
	WorkDate   time.Time        `json:"work_date"`
	Status     AttendanceStatus `json:"status"`
	Notes      *string          `json:"notes,omitempty"`
	RecordedBy uuid.UUID        `json:"recorded_by"`
	CreatedAt  time.Time        `json:"created_at"`
	UpdatedAt  time.Time        `json:"updated_at"`
}

// WageCalculationStatus represents the state of a monthly wage snapshot.
type WageCalculationStatus string

const (
	WageStatusCalculated WageCalculationStatus = "calculated"
	WageStatusBatched    WageCalculationStatus = "batched"
	WageStatusCancelled  WageCalculationStatus = "cancelled"
)

// WageCalculation contains the immutable monthly compensation snapshot.
type WageCalculation struct {
	ID                    uuid.UUID             `json:"id"`
	PropertyID            uuid.UUID             `json:"property_id"`
	StaffID               uuid.UUID             `json:"staff_id"`
	CycleMonth            string                `json:"cycle_month"` // "YYYY-MM"
	BaseMonthlyWagePaise  int64                 `json:"base_monthly_wage_paise"`
	ProratedBaseWagePaise int64                 `json:"prorated_base_wage_paise"`
	TotalBasisDays        int                   `json:"total_basis_days"`
	EmployedBasisDays     int                   `json:"employed_basis_days"`
	DaysPresent           float64               `json:"days_present"`
	DaysPaidLeave         float64               `json:"days_paid_leave"`
	DaysHoliday           float64               `json:"days_holiday"`
	DaysAbsent            float64               `json:"days_absent"`
	DaysUnrecorded        float64               `json:"days_unrecorded"`
	FreeLeaveDaysAllowed  float64               `json:"free_leave_days_allowed"`
	ExcessAbsentDays      float64               `json:"excess_absent_days"`
	PerDayRatePaise       int64                 `json:"per_day_rate_paise"`
	TotalDeductionPaise   int64                 `json:"total_deduction_paise"`
	NetWagePaise          int64                 `json:"net_wage_paise"`
	PayoutItemID          *uuid.UUID            `json:"payout_item_id,omitempty"`
	Status                WageCalculationStatus `json:"status"`
	CalculatedAt          time.Time             `json:"calculated_at"`
	FinalizedBy           uuid.UUID             `json:"finalized_by"`
}

// DailyAttendanceEntry defines a single check-in entry in bulk requests.
type DailyAttendanceEntry struct {
	StaffID uuid.UUID        `json:"staff_id"`
	Status  AttendanceStatus `json:"status"`
	Notes   *string          `json:"notes,omitempty"`
}

// MarkDailyAttendanceRequest is the payload for recording attendance for multiple staff members.
type MarkDailyAttendanceRequest struct {
	WorkDate string                 `json:"work_date"` // "YYYY-MM-DD"
	Entries  []DailyAttendanceEntry `json:"entries"`
}

// LeavePolicyUpdateRequest allows property owners to modify leave policy.
type LeavePolicyUpdateRequest struct {
	MonthlyFreeLeaveDays int              `json:"monthly_free_leave_days"`
	PaidHolidays         []string         `json:"paid_holidays"`
	WorkingDaysBasis     WorkingDaysBasis `json:"working_days_basis"`
}
