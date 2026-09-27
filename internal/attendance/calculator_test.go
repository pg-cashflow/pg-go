package attendance

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestCalculateMonthlyWage_FullAttendance(t *testing.T) {
	staffID := uuid.New()
	propID := uuid.New()

	staff := domain.StaffProfile{
		ID:                   staffID,
		PropertyID:           propID,
		Name:                 "Ramesh Kumar",
		Role:                 "Cook",
		BaseMonthlyWagePaise: 1500000, // Rs 15,000.00
		EffectiveFrom:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Status:               domain.StaffActive,
	}

	policy := domain.LeavePolicy{
		PropertyID:           propID,
		MonthlyFreeLeaveDays: 2,
		WorkingDaysBasis:     domain.WorkingDaysBasisCalendarDays,
	}

	// 30-day month: Sept 2026
	var records []domain.AttendanceRecord
	for day := 1; day <= 30; day++ {
		records = append(records, domain.AttendanceRecord{
			PropertyID: propID,
			StaffID:    staffID,
			WorkDate:   time.Date(2026, 9, day, 0, 0, 0, 0, time.UTC),
			Status:     domain.AttendancePresent,
		})
	}

	calc, err := CalculateMonthlyWage(CalculationParams{
		Staff:      staff,
		Policy:     policy,
		CycleMonth: "2026-09",
		Records:    records,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calc.TotalBasisDays != 30 {
		t.Errorf("expected 30 total basis days, got %d", calc.TotalBasisDays)
	}
	if calc.DaysPresent != 30 {
		t.Errorf("expected 30 days present, got %f", calc.DaysPresent)
	}
	if calc.TotalDeductionPaise != 0 {
		t.Errorf("expected 0 deduction, got %d", calc.TotalDeductionPaise)
	}
	if calc.NetWagePaise != 1500000 {
		t.Errorf("expected 1500000 net wage, got %d", calc.NetWagePaise)
	}
}

func TestCalculateMonthlyWage_AbsencesWithinFreeLeave(t *testing.T) {
	staffID := uuid.New()
	propID := uuid.New()

	staff := domain.StaffProfile{
		ID:                   staffID,
		PropertyID:           propID,
		Name:                 "Sunil Sharma",
		Role:                 "Security",
		BaseMonthlyWagePaise: 1800000, // Rs 18,000.00
		EffectiveFrom:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Status:               domain.StaffActive,
	}

	policy := domain.LeavePolicy{
		PropertyID:           propID,
		MonthlyFreeLeaveDays: 2,
		WorkingDaysBasis:     domain.WorkingDaysBasisCalendarDays,
	}

	// 28 days present, 2 days absent (within 2 free leaves)
	var records []domain.AttendanceRecord
	for day := 1; day <= 28; day++ {
		records = append(records, domain.AttendanceRecord{
			WorkDate: time.Date(2026, 9, day, 0, 0, 0, 0, time.UTC),
			Status:   domain.AttendancePresent,
		})
	}
	records = append(records, domain.AttendanceRecord{
		WorkDate: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC),
		Status:   domain.AttendanceAbsent,
	})
	records = append(records, domain.AttendanceRecord{
		WorkDate: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
		Status:   domain.AttendanceAbsent,
	})

	calc, err := CalculateMonthlyWage(CalculationParams{
		Staff:      staff,
		Policy:     policy,
		CycleMonth: "2026-09",
		Records:    records,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calc.DaysAbsent != 2 {
		t.Errorf("expected 2 days absent, got %f", calc.DaysAbsent)
	}
	if calc.ExcessAbsentDays != 0 {
		t.Errorf("expected 0 excess absent days, got %f", calc.ExcessAbsentDays)
	}
	if calc.TotalDeductionPaise != 0 {
		t.Errorf("expected 0 deduction, got %d", calc.TotalDeductionPaise)
	}
	if calc.NetWagePaise != 1800000 {
		t.Errorf("expected full wage 1800000, got %d", calc.NetWagePaise)
	}
}

func TestCalculateMonthlyWage_ExcessAbsenceDeduction(t *testing.T) {
	staffID := uuid.New()
	propID := uuid.New()

	staff := domain.StaffProfile{
		ID:                   staffID,
		PropertyID:           propID,
		Name:                 "Mohan Lal",
		Role:                 "Housekeeping",
		BaseMonthlyWagePaise: 1500000, // Rs 15,000.00 / 30 = Rs 500/day = 50,000 paise/day
		EffectiveFrom:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Status:               domain.StaffActive,
	}

	policy := domain.LeavePolicy{
		PropertyID:           propID,
		MonthlyFreeLeaveDays: 2,
		WorkingDaysBasis:     domain.WorkingDaysBasisCalendarDays,
	}

	// 5 days absent, 25 days present -> 3 excess days = 3 * 50,000 = 150,000 paise deduction
	var records []domain.AttendanceRecord
	for day := 1; day <= 25; day++ {
		records = append(records, domain.AttendanceRecord{
			WorkDate: time.Date(2026, 9, day, 0, 0, 0, 0, time.UTC),
			Status:   domain.AttendancePresent,
		})
	}
	for day := 26; day <= 30; day++ {
		records = append(records, domain.AttendanceRecord{
			WorkDate: time.Date(2026, 9, day, 0, 0, 0, 0, time.UTC),
			Status:   domain.AttendanceAbsent,
		})
	}

	calc, err := CalculateMonthlyWage(CalculationParams{
		Staff:      staff,
		Policy:     policy,
		CycleMonth: "2026-09",
		Records:    records,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calc.DaysAbsent != 5 {
		t.Errorf("expected 5 days absent, got %f", calc.DaysAbsent)
	}
	if calc.ExcessAbsentDays != 3 {
		t.Errorf("expected 3 excess absent days, got %f", calc.ExcessAbsentDays)
	}
	expectedPerDay := int64(50000)
	if calc.PerDayRatePaise != expectedPerDay {
		t.Errorf("expected 50000 per day rate, got %d", calc.PerDayRatePaise)
	}
	expectedDeduction := int64(150000)
	if calc.TotalDeductionPaise != expectedDeduction {
		t.Errorf("expected %d deduction, got %d", expectedDeduction, calc.TotalDeductionPaise)
	}
	expectedNet := int64(1500000 - 150000)
	if calc.NetWagePaise != expectedNet {
		t.Errorf("expected %d net wage, got %d", expectedNet, calc.NetWagePaise)
	}

	// Double-entry invariant
	if calc.NetWagePaise+calc.TotalDeductionPaise != calc.ProratedBaseWagePaise {
		t.Errorf("invariant violated: net (%d) + deduction (%d) != base (%d)",
			calc.NetWagePaise, calc.TotalDeductionPaise, calc.ProratedBaseWagePaise)
	}
}

func TestCalculateMonthlyWage_MidCycleHireProration(t *testing.T) {
	staffID := uuid.New()
	propID := uuid.New()

	// Staff joined on Sept 16, 2026 (employed 15 days out of 30)
	staff := domain.StaffProfile{
		ID:                   staffID,
		PropertyID:           propID,
		Name:                 "Amit Verma",
		Role:                 "Electrician",
		BaseMonthlyWagePaise: 2000000, // Rs 20,000 / month
		EffectiveFrom:        time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC),
		Status:               domain.StaffActive,
	}

	policy := domain.LeavePolicy{
		PropertyID:           propID,
		MonthlyFreeLeaveDays: 2,
		WorkingDaysBasis:     domain.WorkingDaysBasisCalendarDays,
	}

	// 15 days present in active window
	var records []domain.AttendanceRecord
	for day := 16; day <= 30; day++ {
		records = append(records, domain.AttendanceRecord{
			WorkDate: time.Date(2026, 9, day, 0, 0, 0, 0, time.UTC),
			Status:   domain.AttendancePresent,
		})
	}

	calc, err := CalculateMonthlyWage(CalculationParams{
		Staff:      staff,
		Policy:     policy,
		CycleMonth: "2026-09",
		Records:    records,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calc.TotalBasisDays != 30 {
		t.Errorf("expected 30 total basis days, got %d", calc.TotalBasisDays)
	}
	if calc.EmployedBasisDays != 15 {
		t.Errorf("expected 15 employed basis days, got %d", calc.EmployedBasisDays)
	}
	expectedProrated := int64(1000000) // Rs 10,000
	if calc.ProratedBaseWagePaise != expectedProrated {
		t.Errorf("expected %d prorated wage, got %d", expectedProrated, calc.ProratedBaseWagePaise)
	}
	if calc.NetWagePaise != expectedProrated {
		t.Errorf("expected %d net wage, got %d", expectedProrated, calc.NetWagePaise)
	}
}

func TestCalculateMonthlyWage_FloorGuardNoNegativeWage(t *testing.T) {
	staffID := uuid.New()
	propID := uuid.New()

	staff := domain.StaffProfile{
		ID:                   staffID,
		PropertyID:           propID,
		Name:                 "Absent Worker",
		Role:                 "Cleaner",
		BaseMonthlyWagePaise: 1200000, // Rs 12,000
		EffectiveFrom:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Status:               domain.StaffActive,
	}

	policy := domain.LeavePolicy{
		PropertyID:           propID,
		MonthlyFreeLeaveDays: 0,
		WorkingDaysBasis:     domain.WorkingDaysBasisCalendarDays,
	}

	// Absent all 30 days
	var records []domain.AttendanceRecord
	for day := 1; day <= 30; day++ {
		records = append(records, domain.AttendanceRecord{
			WorkDate: time.Date(2026, 9, day, 0, 0, 0, 0, time.UTC),
			Status:   domain.AttendanceAbsent,
		})
	}

	calc, err := CalculateMonthlyWage(CalculationParams{
		Staff:      staff,
		Policy:     policy,
		CycleMonth: "2026-09",
		Records:    records,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calc.NetWagePaise < 0 {
		t.Errorf("net wage must not be negative: %d", calc.NetWagePaise)
	}
	if calc.NetWagePaise != 0 {
		t.Errorf("expected 0 net wage for 30 days absence with 0 free leave, got %d", calc.NetWagePaise)
	}
	if calc.TotalDeductionPaise != calc.ProratedBaseWagePaise {
		t.Errorf("deduction should cap at base wage: got %d, base %d",
			calc.TotalDeductionPaise, calc.ProratedBaseWagePaise)
	}
}

func TestCalculateMonthlyWage_PaidHolidaysAndHalfDays(t *testing.T) {
	staffID := uuid.New()
	propID := uuid.New()

	staff := domain.StaffProfile{
		ID:                   staffID,
		PropertyID:           propID,
		Name:                 "Deepak",
		Role:                 "Warden",
		BaseMonthlyWagePaise: 3000000, // Rs 30,000 / 30 = Rs 1,000/day
		EffectiveFrom:        time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Status:               domain.StaffActive,
	}

	policy := domain.LeavePolicy{
		PropertyID:           propID,
		MonthlyFreeLeaveDays: 1,
		PaidHolidays:         []string{"2026-09-05"}, // Sept 5 Teachers day holiday
		WorkingDaysBasis:     domain.WorkingDaysBasisCalendarDays,
	}

	// 27 full present, 1 holiday (Sept 5), 2 half days (Sept 28, 29)
	var records []domain.AttendanceRecord
	for day := 1; day <= 27; day++ {
		if day == 5 {
			records = append(records, domain.AttendanceRecord{
				WorkDate: time.Date(2026, 9, day, 0, 0, 0, 0, time.UTC),
				Status:   domain.AttendanceHoliday,
			})
		} else {
			records = append(records, domain.AttendanceRecord{
				WorkDate: time.Date(2026, 9, day, 0, 0, 0, 0, time.UTC),
				Status:   domain.AttendancePresent,
			})
		}
	}
	// 2 half days: 2 * 0.5 = 1.0 absent
	records = append(records, domain.AttendanceRecord{
		WorkDate: time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC),
		Status:   domain.AttendanceHalfDay,
	})
	records = append(records, domain.AttendanceRecord{
		WorkDate: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC),
		Status:   domain.AttendanceHalfDay,
	})
	records = append(records, domain.AttendanceRecord{
		WorkDate: time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC),
		Status:   domain.AttendancePresent,
	})

	calc, err := CalculateMonthlyWage(CalculationParams{
		Staff:      staff,
		Policy:     policy,
		CycleMonth: "2026-09",
		Records:    records,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if calc.DaysHoliday != 1 {
		t.Errorf("expected 1 holiday, got %f", calc.DaysHoliday)
	}
	if calc.DaysAbsent != 1.0 {
		t.Errorf("expected 1.0 day absent from 2 half days, got %f", calc.DaysAbsent)
	}
	// 1 absent day <= 1 free leave day allowed -> 0 excess
	if calc.ExcessAbsentDays != 0 {
		t.Errorf("expected 0 excess absent days, got %f", calc.ExcessAbsentDays)
	}
	if calc.NetWagePaise != 3000000 {
		t.Errorf("expected full wage 3000000, got %d", calc.NetWagePaise)
	}
}
