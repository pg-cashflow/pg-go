package billing

import (
	"testing"
	"time"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestProrateAmountFullPeriod(t *testing.T) {
	got := domain.ProrateAmount(30000, 30, 30)
	if got != 30000 {
		t.Fatalf("got %d", got)
	}
}

func TestProrateAmountHalf(t *testing.T) {
	// floor(30000 * 15 / 30) = 15000
	got := domain.ProrateAmount(30000, 15, 30)
	if got != 15000 {
		t.Fatalf("got %d", got)
	}
}

func TestProrateAmountFloor(t *testing.T) {
	// floor(10000 * 1 / 31) = 322
	got := domain.ProrateAmount(10000, 1, 31)
	if got != 322 {
		t.Fatalf("got %d want 322", got)
	}
}

func TestProrateAmountZeroOccupied(t *testing.T) {
	if domain.ProrateAmount(10000, 0, 30) != 0 {
		t.Fatal("expected 0")
	}
}

func TestDaysInPeriodInclusive(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	if domain.DaysInPeriod(start, end) != 31 {
		t.Fatalf("got %d", domain.DaysInPeriod(start, end))
	}
}

func TestProrateMidMonthVacate(t *testing.T) {
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 31, 0, 0, 0, 0, time.UTC)
	vacate := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	daysOcc := domain.DaysInPeriod(start, vacate)
	daysPer := domain.DaysInPeriod(start, end)
	if daysOcc != 10 || daysPer != 31 {
		t.Fatalf("daysOcc=%d daysPer=%d", daysOcc, daysPer)
	}
	got := domain.ProrateAmount(31000, daysOcc, daysPer)
	if got != 10000 {
		t.Fatalf("got %d want 10000", got)
	}
}
