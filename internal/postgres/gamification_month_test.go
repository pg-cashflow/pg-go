package postgres

import (
	"testing"
	"time"
)

func TestISTMonthBounds(t *testing.T) {
	from, to, err := istMonthBounds("2026-10")
	if err != nil {
		t.Fatalf("istMonthBounds: %v", err)
	}
	wantFrom := time.Date(2026, 9, 30, 18, 30, 0, 0, time.UTC)
	wantTo := time.Date(2026, 10, 31, 18, 30, 0, 0, time.UTC)
	if !from.Equal(wantFrom) || !to.Equal(wantTo) {
		t.Fatalf("got [%s, %s), want [%s, %s)", from.UTC(), to.UTC(), wantFrom, wantTo)
	}
	// December rolls into the next year.
	_, to, err = istMonthBounds("2026-12")
	if err != nil || !to.Equal(time.Date(2026, 12, 31, 18, 30, 0, 0, time.UTC)) {
		t.Fatalf("december end = %s, err %v", to.UTC(), err)
	}
	if _, _, err := istMonthBounds("2026-13"); err == nil {
		t.Fatalf("invalid month must return an error")
	}
}
