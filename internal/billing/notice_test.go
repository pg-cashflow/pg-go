package billing

import (
	"testing"
	"time"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestNoticeDaysGivenNil(t *testing.T) {
	if NoticeDaysGiven(nil, time.Now()) != 0 {
		t.Fatal("expected 0")
	}
}

func TestPolicyMetEdgeCases(t *testing.T) {
	cases := []struct {
		name       string
		noticeDays int
		period     int16
		want       bool
	}{
		{"29_of_30", 29, 30, false},
		{"30_of_30", 30, 30, true},
		{"31_of_30", 31, 30, true},
		{"0_of_30", 0, 30, false},
		{"30_of_0", 30, 0, true}, // zero period always met
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PolicyMet(tc.noticeDays, tc.period); got != tc.want {
				t.Fatalf("PolicyMet(%d,%d)=%v want %v", tc.noticeDays, tc.period, got, tc.want)
			}
		})
	}
}

func TestNoticeCompliance29_30_31(t *testing.T) {
	given := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	tenant := &domain.Tenant{NoticePeriodDays: 30, NoticeGivenAt: &given}

	asOf29 := given.Add(29 * 24 * time.Hour)
	days, met := NoticeCompliance(tenant, asOf29)
	if days != 29 || met {
		t.Fatalf("29d: days=%d met=%v", days, met)
	}

	asOf30 := given.Add(30 * 24 * time.Hour)
	days, met = NoticeCompliance(tenant, asOf30)
	if days != 30 || !met {
		t.Fatalf("30d: days=%d met=%v", days, met)
	}

	asOf31 := given.Add(31 * 24 * time.Hour)
	days, met = NoticeCompliance(tenant, asOf31)
	if days != 31 || !met {
		t.Fatalf("31d: days=%d met=%v", days, met)
	}
}

func TestSuggestedRefund(t *testing.T) {
	if SuggestedRefundPaise(50000, false) != 0 {
		t.Fatal("expected 0 when policy not met")
	}
	if SuggestedRefundPaise(50000, true) != 50000 {
		t.Fatal("expected full original when policy met")
	}
}
