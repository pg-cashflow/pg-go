package timeutil

import (
	"sync"
	"testing"
	"time"
)

func TestSystemClock_Now(t *testing.T) {
	clk := System
	now := clk.Now()
	if now.Location() != time.UTC {
		t.Errorf("expected UTC location, got %v", now.Location())
	}
	diff := time.Since(now)
	if diff < 0 || diff > 5*time.Second {
		t.Errorf("system clock drift out of bounds: %v", diff)
	}
}

func TestFixedClock_NormalizesToUTC(t *testing.T) {
	istLoc := time.FixedZone("IST", 5*3600+30*60)
	// 2028 is a leap year. 2028-03-01 00:00:00 IST equals 2028-02-29 18:30:00 UTC.
	istTime := time.Date(2028, time.March, 1, 0, 0, 0, 0, istLoc)
	clk := NewFixedClock(istTime)

	got := clk.Now()
	if got.Location() != time.UTC {
		t.Errorf("expected UTC location, got %v", got.Location())
	}
	if got.Year() != 2028 || got.Month() != time.February || got.Day() != 29 {
		t.Errorf("expected 2028-02-29, got %04d-%02d-%02d", got.Year(), got.Month(), got.Day())
	}
	if got.Hour() != 18 || got.Minute() != 30 {
		t.Errorf("expected 18:30 UTC, got %02d:%02d", got.Hour(), got.Minute())
	}
}

func TestFixedClock_SetAndAdvance(t *testing.T) {
	base := time.Date(2025, time.January, 1, 12, 0, 0, 0, time.UTC)
	clk := NewFixedClock(base)

	if !clk.Now().Equal(base) {
		t.Fatalf("expected %v, got %v", base, clk.Now())
	}

	clk.Advance(2 * time.Hour)
	expectedAfterAdvance := time.Date(2025, time.January, 1, 14, 0, 0, 0, time.UTC)
	if !clk.Now().Equal(expectedAfterAdvance) {
		t.Fatalf("expected %v, got %v", expectedAfterAdvance, clk.Now())
	}

	newTime := time.Date(2026, time.June, 15, 8, 30, 0, 0, time.UTC)
	clk.Set(newTime)
	if !clk.Now().Equal(newTime) {
		t.Fatalf("expected %v, got %v", newTime, clk.Now())
	}
}

func TestFixedClock_Concurrency(t *testing.T) {
	base := time.Date(2025, time.January, 1, 0, 0, 0, 0, time.UTC)
	clk := NewFixedClock(base)

	const workers = 20
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if workerID%4 == 0 {
					clk.Advance(time.Millisecond)
				} else if workerID%4 == 1 {
					clk.Set(base.Add(time.Duration(j) * time.Second))
				} else {
					_ = clk.Now()
				}
			}
		}(i)
	}

	wg.Wait()
}
