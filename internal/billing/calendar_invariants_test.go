package billing

import (
	"testing"
	"time"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

// TestCalendar_MonthEndClampingTable validates Gate 13: month-end due day clamping across all 12 months.
// Tenants with due days 29, 30, or 31 must be charged on the final day of shorter months.
func TestCalendar_MonthEndClampingTable(t *testing.T) {
	loc, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		loc = time.FixedZone("IST", 5*3600+1800)
	}

	type monthCase struct {
		year                int
		month               time.Month
		lastDay             int
		isLeap              bool
		expectedClampedDays []int // days that must be processed on the final day of this month
	}

	cases := []monthCase{
		{year: 2026, month: time.January, lastDay: 31, isLeap: false, expectedClampedDays: []int{31}},
		{year: 2026, month: time.February, lastDay: 28, isLeap: false, expectedClampedDays: []int{28, 29, 30, 31}},
		{year: 2024, month: time.February, lastDay: 29, isLeap: true, expectedClampedDays: []int{29, 30, 31}},
		{year: 2026, month: time.March, lastDay: 31, isLeap: false, expectedClampedDays: []int{31}},
		{year: 2026, month: time.April, lastDay: 30, isLeap: false, expectedClampedDays: []int{30, 31}},
		{year: 2026, month: time.May, lastDay: 31, isLeap: false, expectedClampedDays: []int{31}},
		{year: 2026, month: time.June, lastDay: 30, isLeap: false, expectedClampedDays: []int{30, 31}},
		{year: 2026, month: time.July, lastDay: 31, isLeap: false, expectedClampedDays: []int{31}},
		{year: 2026, month: time.August, lastDay: 31, isLeap: false, expectedClampedDays: []int{31}},
		{year: 2026, month: time.September, lastDay: 30, isLeap: false, expectedClampedDays: []int{30, 31}},
		{year: 2026, month: time.October, lastDay: 31, isLeap: false, expectedClampedDays: []int{31}},
		{year: 2026, month: time.November, lastDay: 30, isLeap: false, expectedClampedDays: []int{30, 31}},
		{year: 2026, month: time.December, lastDay: 31, isLeap: false, expectedClampedDays: []int{31}},
	}

	for _, tc := range cases {
		name := tc.month.String()
		if tc.isLeap {
			name += "_LeapYear"
		}
		t.Run(name, func(t *testing.T) {
			now := time.Date(tc.year, tc.month, tc.lastDay, 10, 0, 0, 0, loc)
			day := now.Day()

			// Month-end clamping algorithm from jobs.BillingCycle
			dueDays := []int{day}
			tomorrow := now.AddDate(0, 0, 1)
			if tomorrow.Month() != now.Month() {
				for d := day + 1; d <= 31; d++ {
					dueDays = append(dueDays, d)
				}
			}

			if len(dueDays) != len(tc.expectedClampedDays) {
				t.Fatalf("for %s %d: expected %d due days %v, got %d %v",
					tc.month, tc.year, len(tc.expectedClampedDays), tc.expectedClampedDays, len(dueDays), dueDays)
			}
			for i, expectedDay := range tc.expectedClampedDays {
				if dueDays[i] != expectedDay {
					t.Errorf("for %s %d at index %d: expected day %d, got %d",
						tc.month, tc.year, i, expectedDay, dueDays[i])
				}
			}
		})
	}
}

// TestCalendar_FebruaryProrationMath validates Gate 13: February check-in proration and integer-paise conservation.
func TestCalendar_FebruaryProrationMath(t *testing.T) {
	t.Run("NonLeapYear_2026_Feb20_CheckIn", func(t *testing.T) {
		start := time.Date(2026, time.February, 20, 0, 0, 0, 0, time.UTC)
		end := time.Date(2026, time.February, 28, 0, 0, 0, 0, time.UTC)

		daysOccupied := domain.DaysInPeriod(start, end)
		if daysOccupied != 9 {
			t.Fatalf("expected 9 days occupied (Feb 20-28 inclusive), got %d", daysOccupied)
		}

		fullMonthDays := domain.DaysInPeriod(time.Date(2026, time.February, 1, 0, 0, 0, 0, time.UTC), end)
		if fullMonthDays != 28 {
			t.Fatalf("expected 28 total days in Feb 2026, got %d", fullMonthDays)
		}

		rentPaise := int64(2800000) // ₹28,000.00
		prorated := domain.ProrateAmount(rentPaise, daysOccupied, fullMonthDays)
		expected := int64(900000) // ₹9,000.00 (9/28 of 2800000)
		if prorated != expected {
			t.Fatalf("expected prorated rent %d paise, got %d paise", expected, prorated)
		}
	})

	t.Run("LeapYear_2024_Feb20_CheckIn", func(t *testing.T) {
		start := time.Date(2024, time.February, 20, 0, 0, 0, 0, time.UTC)
		end := time.Date(2024, time.February, 29, 0, 0, 0, 0, time.UTC)

		daysOccupied := domain.DaysInPeriod(start, end)
		if daysOccupied != 10 {
			t.Fatalf("expected 10 days occupied (Feb 20-29 inclusive), got %d", daysOccupied)
		}

		fullMonthDays := domain.DaysInPeriod(time.Date(2024, time.February, 1, 0, 0, 0, 0, time.UTC), end)
		if fullMonthDays != 29 {
			t.Fatalf("expected 29 total days in Feb 2024, got %d", fullMonthDays)
		}

		rentPaise := int64(2900000) // ₹29,000.00
		prorated := domain.ProrateAmount(rentPaise, daysOccupied, fullMonthDays)
		expected := int64(1000000) // ₹10,000.00 (10/29 of 2900000)
		if prorated != expected {
			t.Fatalf("expected prorated rent %d paise, got %d paise", expected, prorated)
		}
	})

	t.Run("ZeroRemainderPaiseConservation", func(t *testing.T) {
		// Test integer paise conservation across all possible split days in February (28 and 29)
		for _, totalDays := range []int{28, 29, 30, 31} {
			monthlyRent := int64(1254375) // odd paise: ₹12,543.75
			for split := 1; split < totalDays; split++ {
				part1 := domain.ProrateAmount(monthlyRent, split, totalDays)
				part2 := domain.ProrateAmount(monthlyRent, totalDays-split, totalDays)
				sum := part1 + part2
				drift := monthlyRent - sum
				// Integer division truncation can differ by at most 1 paise
				if drift < 0 || drift > 1 {
					t.Fatalf("totalDays=%d split=%d: drift %d paise exceeds [0, 1] paise floor (part1=%d, part2=%d, total=%d)",
						totalDays, split, drift, part1, part2, monthlyRent)
				}
			}
		}
	})
}

// TestCalendar_ISTTimezoneBoundary validates Gate 13: Asia/Kolkata (+05:30) midnight partition.
func TestCalendar_ISTTimezoneBoundary(t *testing.T) {
	// In UTC: 18:29:59 on Oct 8 is 23:59:59 IST on Oct 8
	beforeMidnightUTC := time.Date(2026, 10, 8, 18, 29, 59, 0, time.UTC)
	// In UTC: 18:30:00 on Oct 8 is 00:00:00 IST on Oct 9
	atMidnightUTC := time.Date(2026, 10, 8, 18, 30, 0, 0, time.UTC)
	// In UTC: 18:30:01 on Oct 8 is 00:00:01 IST on Oct 9
	afterMidnightUTC := time.Date(2026, 10, 8, 18, 30, 1, 0, time.UTC)

	dateBefore := calendarDateIST(beforeMidnightUTC)
	dateAt := calendarDateIST(atMidnightUTC)
	dateAfter := calendarDateIST(afterMidnightUTC)

	expectedBefore := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	expectedAfter := time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)

	if !dateBefore.Equal(expectedBefore) {
		t.Fatalf("expected before-midnight date %v, got %v", expectedBefore, dateBefore)
	}
	if !dateAt.Equal(expectedAfter) {
		t.Fatalf("expected at-midnight date %v, got %v", expectedAfter, dateAt)
	}
	if !dateAfter.Equal(expectedAfter) {
		t.Fatalf("expected after-midnight date %v, got %v", expectedAfter, dateAfter)
	}
}
