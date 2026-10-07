package gamification

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestCashCreditValuePaise(t *testing.T) {
	cases := []struct {
		name     string
		meta     string
		cost     int
		pointVal int64
		want     int64
		wantErr  bool
	}{
		{"seed reward: 500 points, Rs 500", `{"discount_paise": 50000}`, 500, 100, 50_000, false},
		{"typo above points value is clamped", `{"discount_paise": 5000000}`, 500, 100, 50_000, false},
		{"discount below points value is kept", `{"discount_paise": 20000}`, 500, 100, 20_000, false},
		{"no metadata uses points value, not a fixed default", ``, 100, 100, 10_000, false},
		{"zero discount uses points value", `{"discount_paise": 0}`, 100, 100, 10_000, false},
		{"null metadata uses points value", `null`, 100, 100, 10_000, false},
		{"negative discount is refused", `{"discount_paise": -1}`, 500, 100, 0, true},
		{"unreadable metadata is refused", `{"discount_paise": "abc"`, 500, 100, 0, true},
		{"zero point value is refused", `{"discount_paise": 50000}`, 500, 0, 0, true},
		{"zero points cost is refused", `{"discount_paise": 50000}`, 0, 100, 0, true},
	}
	for _, c := range cases {
		got, err := cashCreditValuePaise(json.RawMessage(c.meta), c.cost, c.pointVal)
		if c.wantErr {
			if !errors.Is(err, ErrInvalidRewardValue) {
				t.Errorf("%s: err = %v, want ErrInvalidRewardValue", c.name, err)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%s: got %d, err %v; want %d", c.name, got, err, c.want)
		}
	}
}

// Monthly caps use IST calendar months. 20:00 UTC on 30 Sep is 01:30 IST on 1 Oct.
func TestMonthKeyUsesIST(t *testing.T) {
	cases := []struct {
		at   time.Time
		want string
	}{
		{time.Date(2026, 9, 30, 20, 0, 0, 0, time.UTC), "2026-10"},
		{time.Date(2026, 9, 30, 18, 29, 59, 0, time.UTC), "2026-09"},
		{time.Date(2026, 9, 30, 18, 30, 0, 0, time.UTC), "2026-10"},
		{time.Date(2026, 12, 31, 19, 0, 0, 0, time.UTC), "2027-01"},
	}
	for _, c := range cases {
		if got := monthKey(c.at); got != c.want {
			t.Errorf("monthKey(%s) = %s, want %s", c.at.Format(time.RFC3339), got, c.want)
		}
	}
}
