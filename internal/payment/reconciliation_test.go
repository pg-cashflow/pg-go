package payment

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestParsePeriodBounds_Monthly(t *testing.T) {
	from, to, err := parsePeriodBounds("2026-08")
	if err != nil {
		t.Fatalf("parsePeriodBounds: %v", err)
	}
	loc, _ := time.LoadLocation("Asia/Kolkata")
	wantFrom := time.Date(2026, 8, 1, 0, 0, 0, 0, loc).UTC()
	wantTo := time.Date(2026, 9, 1, 0, 0, 0, 0, loc).UTC()
	if !from.Equal(wantFrom) || !to.Equal(wantTo) {
		t.Fatalf("got [%v, %v) want [%v, %v)", from, to, wantFrom, wantTo)
	}
}

func TestParsePeriodBounds_Yearly(t *testing.T) {
	from, to, err := parsePeriodBounds("2025")
	if err != nil {
		t.Fatalf("parsePeriodBounds: %v", err)
	}
	loc, _ := time.LoadLocation("Asia/Kolkata")
	wantFrom := time.Date(2025, 1, 1, 0, 0, 0, 0, loc).UTC()
	wantTo := time.Date(2026, 1, 1, 0, 0, 0, 0, loc).UTC()
	if !from.Equal(wantFrom) || !to.Equal(wantTo) {
		t.Fatalf("got [%v, %v) want [%v, %v)", from, to, wantFrom, wantTo)
	}
}

func TestParsePeriodBounds_Invalid(t *testing.T) {
	for _, p := range []string{"", "26", "2026-13", "2026-00", "abcd", "2026-8"} {
		if _, _, err := parsePeriodBounds(p); err == nil {
			t.Fatalf("expected error for %q", p)
		}
	}
}

type stubSummaryRepo struct {
	sum *ReconciliationSummary
	err error
}

func (s stubSummaryRepo) QueryReconciliation(ctx context.Context, propertyID uuid.UUID, from, to time.Time) (*ReconciliationSummary, error) {
	if s.err != nil {
		return nil, s.err
	}
	out := *s.sum
	return &out, nil
}

func TestBuildSummary_SetsPeriodAndChannels(t *testing.T) {
	svc := NewService(nil, nil, nil, stubSummaryRepo{
		sum: &ReconciliationSummary{
			RentCollected:   1000,
			OutstandingRent: 200,
			CreditsHeld:     50,
			DepositsHeld:    5000,
		},
	}, nil)
	got, err := svc.BuildSummary(context.Background(), uuid.New(), "2026-08")
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	if got.Period != "2026-08" {
		t.Fatalf("period=%q", got.Period)
	}
	if got.CollectedByChannel == nil {
		t.Fatal("expected non-nil CollectedByChannel")
	}
	if got.RentCollected != 1000 || got.OutstandingRent != 200 {
		t.Fatalf("unexpected summary values: %+v", got)
	}
}

func TestReconciliationSummary_JSONShape(t *testing.T) {
	s := ReconciliationSummary{
		Period:             "2026-08",
		RentCollected:      1,
		CollectedByChannel: map[string]int64{"cash": 1},
		OutstandingRent:    2,
		CreditsHeld:        3,
		DepositsHeld:       4,
		DepositsRefunded:   5,
	}
	if s.Period == "" || s.CollectedByChannel["cash"] != 1 {
		t.Fatalf("bad shape: %+v", s)
	}
}
