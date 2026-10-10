package attendance

import (
	"fmt"
	"math"
	"time"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

// CalculationParams holds the input parameters required to calculate monthly wages for a staff member.
type CalculationParams struct {
	Staff      domain.StaffProfile
	Policy     domain.LeavePolicy
	CycleMonth string // "YYYY-MM"
	Records    []domain.AttendanceRecord
}

// CalculateMonthlyWage performs pure integer-paise wage calculation with attendance adjustments,
// leave allowances, and mid-cycle hire/departure proration.
func CalculateMonthlyWage(params CalculationParams) (*domain.WageCalculation, error) {
	cycleTime, err := time.Parse("2006-01", params.CycleMonth)
	if err != nil {
		return nil, fmt.Errorf("invalid cycle_month format: %w", err)
	}

	year, month, _ := cycleTime.Date()
	cycleStart := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	cycleEnd := cycleStart.AddDate(0, 1, -1)
	calendarDaysInMonth := cycleEnd.Day()

	// 1. Determine Total Basis Days in cycle based on policy
	totalBasisDays := computeTotalBasisDays(cycleStart, cycleEnd, params.Policy.WorkingDaysBasis)
	if totalBasisDays <= 0 {
		totalBasisDays = calendarDaysInMonth
	}

	// 2. Determine Active Employed Window in this month
	staffStart := normalizeDate(params.Staff.EffectiveFrom)
	windowStart := cycleStart
	if staffStart.After(cycleStart) {
		windowStart = staffStart
	}

	windowEnd := cycleEnd
	if params.Staff.EffectiveTo != nil {
		staffEnd := normalizeDate(*params.Staff.EffectiveTo)
		if staffEnd.Before(cycleEnd) {
			windowEnd = staffEnd
		}
	}

	// Check if staff was active during this cycle
	if windowStart.After(cycleEnd) || windowEnd.Before(cycleStart) || windowStart.After(windowEnd) || params.Staff.Status == domain.StaffInactive && params.Staff.EffectiveTo != nil && params.Staff.EffectiveTo.Before(cycleStart) {
		return &domain.WageCalculation{
			PropertyID:            params.Staff.PropertyID,
			StaffID:               params.Staff.ID,
			CycleMonth:            params.CycleMonth,
			BaseMonthlyWagePaise:  params.Staff.BaseMonthlyWagePaise,
			ProratedBaseWagePaise: 0,
			TotalBasisDays:        totalBasisDays,
			EmployedBasisDays:     0,
			DaysPresent:           0,
			DaysPaidLeave:         0,
			DaysHoliday:           0,
			DaysAbsent:            0,
			FreeLeaveDaysAllowed:  0,
			ExcessAbsentDays:      0,
			PerDayRatePaise:       params.Staff.BaseMonthlyWagePaise / int64(totalBasisDays),
			TotalDeductionPaise:   0,
			NetWagePaise:          0,
			Status:                domain.WageStatusCalculated,
			CalculatedAt:          time.Now().UTC(),
		}, nil
	}

	// 3. Compute Employed Basis Days
	employedBasisDays := computeEmployedBasisDays(windowStart, windowEnd, cycleStart, cycleEnd, totalBasisDays, params.Policy.WorkingDaysBasis)
	if employedBasisDays > totalBasisDays {
		employedBasisDays = totalBasisDays
	}

	// 4. Compute Prorated Base Wage & Per-Day Rate
	var proratedBaseWagePaise int64
	if windowStart.Equal(cycleStart) && windowEnd.Equal(cycleEnd) {
		proratedBaseWagePaise = params.Staff.BaseMonthlyWagePaise
	} else {
		proratedBaseWagePaise = (params.Staff.BaseMonthlyWagePaise * int64(employedBasisDays)) / int64(totalBasisDays)
	}

	perDayRatePaise := params.Staff.BaseMonthlyWagePaise / int64(totalBasisDays)
	if perDayRatePaise <= 0 {
		perDayRatePaise = 1
	}

	// 5. Aggregate Attendance Records within the active window
	recordMap := make(map[string]domain.AttendanceRecord)
	for _, rec := range params.Records {
		dStr := normalizeDate(rec.WorkDate).Format("2006-01-02")
		recordMap[dStr] = rec
	}

	holidaySet := make(map[string]bool)
	for _, h := range params.Policy.PaidHolidays {
		holidaySet[h] = true
	}

	var daysPresent, daysPaidLeave, daysHoliday, daysAbsent, daysUnrecorded float64

	// Walk every calendar day in the employed window
	curr := windowStart
	for !curr.After(windowEnd) {
		dStr := curr.Format("2006-01-02")
		rec, exists := recordMap[dStr]

		if exists {
			switch rec.Status {
			case domain.AttendancePresent:
				daysPresent += 1.0
			case domain.AttendancePaidLeave:
				daysPaidLeave += 1.0
			case domain.AttendanceHoliday:
				daysHoliday += 1.0
			case domain.AttendanceHalfDay:
				daysPresent += 0.5
				daysAbsent += 0.5
			case domain.AttendanceAbsent:
				daysAbsent += 1.0
			default:
				daysAbsent += 1.0
			}
		} else {
			// Unrecorded day handling: check if holiday or rest day
			if holidaySet[dStr] {
				daysHoliday += 1.0
			} else if params.Policy.WorkingDaysBasis == domain.WorkingDaysBasisExcludingSundays && curr.Weekday() == time.Sunday {
				// Sunday rest day under excluding_sundays basis: not counted as absent
			} else {
				// Absence if not recorded — tracked explicitly as unrecorded so owner has visibility
				daysAbsent += 1.0
				daysUnrecorded += 1.0
			}
		}

		curr = curr.AddDate(0, 0, 1)
	}

	// 6. Free Leave Days Allowed (pro-rated for partial employment)
	freeLeaveDaysAllowed := float64(params.Policy.MonthlyFreeLeaveDays)
	if totalBasisDays > 0 && employedBasisDays < totalBasisDays {
		freeLeaveDaysAllowed = (float64(params.Policy.MonthlyFreeLeaveDays) * float64(employedBasisDays)) / float64(totalBasisDays)
		freeLeaveDaysAllowed = math.Round(freeLeaveDaysAllowed*10) / 10 // round to 1 decimal
	}

	// 7. Excess Absent Days
	var excessAbsentDays float64
	if daysAbsent > freeLeaveDaysAllowed {
		excessAbsentDays = daysAbsent - freeLeaveDaysAllowed
	}

	// 8. Deductions and Net Wage with pure integer half-day arithmetic (Zero Floats in money calculations)
	// Half-days are converted to an integer half-day count (2 units per day).
	excessHalfDays := int64(math.Round(excessAbsentDays * 2))
	totalDeductionPaise := (excessHalfDays * perDayRatePaise) / 2
	if totalDeductionPaise > proratedBaseWagePaise {
		totalDeductionPaise = proratedBaseWagePaise
	}

	netWagePaise := proratedBaseWagePaise - totalDeductionPaise
	if netWagePaise < 0 {
		netWagePaise = 0
	}

	return &domain.WageCalculation{
		PropertyID:            params.Staff.PropertyID,
		StaffID:               params.Staff.ID,
		CycleMonth:            params.CycleMonth,
		BaseMonthlyWagePaise:  params.Staff.BaseMonthlyWagePaise,
		ProratedBaseWagePaise: proratedBaseWagePaise,
		TotalBasisDays:        totalBasisDays,
		EmployedBasisDays:     employedBasisDays,
		DaysPresent:           daysPresent,
		DaysPaidLeave:         daysPaidLeave,
		DaysHoliday:           daysHoliday,
		DaysAbsent:            daysAbsent,
		DaysUnrecorded:        daysUnrecorded,
		FreeLeaveDaysAllowed:  freeLeaveDaysAllowed,
		ExcessAbsentDays:      excessAbsentDays,
		PerDayRatePaise:       perDayRatePaise,
		TotalDeductionPaise:   totalDeductionPaise,
		NetWagePaise:          netWagePaise,
		Status:                domain.WageStatusCalculated,
		CalculatedAt:          time.Now().UTC(),
	}, nil
}

func computeTotalBasisDays(cycleStart, cycleEnd time.Time, basis domain.WorkingDaysBasis) int {
	switch basis {
	case domain.WorkingDaysBasisFixed30:
		return 30
	case domain.WorkingDaysBasisExcludingSundays:
		count := 0
		curr := cycleStart
		for !curr.After(cycleEnd) {
			if curr.Weekday() != time.Sunday {
				count++
			}
			curr = curr.AddDate(0, 0, 1)
		}
		return count
	case domain.WorkingDaysBasisCalendarDays:
		fallthrough
	default:
		return cycleEnd.Day()
	}
}

func computeEmployedBasisDays(windowStart, windowEnd, cycleStart, cycleEnd time.Time, totalBasisDays int, basis domain.WorkingDaysBasis) int {
	calendarDaysInWindow := int(windowEnd.Sub(windowStart).Hours()/24) + 1
	calendarDaysInMonth := cycleEnd.Day()

	switch basis {
	case domain.WorkingDaysBasisFixed30:
		if windowStart.Equal(cycleStart) && windowEnd.Equal(cycleEnd) {
			return 30
		}
		res := (calendarDaysInWindow * 30) / calendarDaysInMonth
		if res > 30 {
			res = 30
		}
		return res
	case domain.WorkingDaysBasisExcludingSundays:
		count := 0
		curr := windowStart
		for !curr.After(windowEnd) {
			if curr.Weekday() != time.Sunday {
				count++
			}
			curr = curr.AddDate(0, 0, 1)
		}
		return count
	case domain.WorkingDaysBasisCalendarDays:
		fallthrough
	default:
		return calendarDaysInWindow
	}
}

func normalizeDate(t time.Time) time.Time {
	utc := t.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}
