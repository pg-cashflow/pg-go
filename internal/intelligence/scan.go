package intelligence

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

type ScanInput struct {
	PropertyID     uuid.UUID
	Period         string
	Recon          *payment.ReconciliationSummary
	Occupancy      finance.Occupancy
	OutstandingPaise int64
	LoyaltyIssuedPaise int64
	LoyaltyBudgetPaise int64
	MealExpected       map[string]int
	MealPrepared       map[string]int
	MeterAnomalies     []string
	RepeatRepairPaise  int64
}

type Service struct {
	Store finance.Store
	Pub   interface {
		Publish(ctx context.Context, e domain.Event) error
	}
	Now func() time.Time
}

func NewService(store finance.Store) *Service {
	return &Service{Store: store, Now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) Scan(ctx context.Context, in ScanInput) ([]domain.LeakageEvent, []domain.Recommendation, error) {
	var leaks []domain.LeakageEvent
	add := func(cat string, paise int64, sev int, evidence any, issue, action string) {
		b, _ := json.Marshal(evidence)
		e := domain.LeakageEvent{
			ID:             uuid.New(),
			PropertyID:     in.PropertyID,
			Category:       cat,
			EstimatedPaise: paise,
			ConfidenceBPS:  9000,
			Evidence:       b,
			Severity:       sev,
			Status:         "open",
			DetectedAt:     s.Now(),
		}
		_ = s.Store.InsertLeakage(ctx, &e)
		leaks = append(leaks, e)
		if sev >= 2 && issue != "" {
			rec := domain.Recommendation{
				ID:                   uuid.New(),
				PropertyID:           in.PropertyID,
				LeakageEventID:       &e.ID,
				Issue:                issue,
				SuggestedAction:      action,
				ExpectedSavingsPaise: paise,
				Effort:               "low",
				Risk:                 "low",
				ConfidenceBPS:        8500,
				SafetyOK:             true,
				QualityOK:            true,
				Status:               "detected",
				CreatedAt:            s.Now(),
			}
			_ = s.Store.InsertRecommendation(ctx, &rec)
		}
	}

	if in.Occupancy.CapacityBeds > 0 {
		vacant := in.Occupancy.CapacityBeds - in.Occupancy.OccupiedBeds
		if vacant > 0 {
			est := int64(vacant) * 630000
			add("vacancy", est, 2, map[string]any{"vacant_beds": vacant, "beds_at_risk": in.Occupancy.BedsAtRisk},
				"Vacant beds below capacity", "Market vacant beds; notice_given_at is a dated future vacancy")
		}
		if in.Occupancy.BedsAtRisk > 0 {
			add("notice_vacancy", int64(in.Occupancy.BedsAtRisk)*630000, 1,
				map[string]any{"beds_at_risk": in.Occupancy.BedsAtRisk},
				"Tenants on notice (deterministic vacancy)", "Backfill beds by notice end date")
		}
	}
	if in.Recon != nil && in.Recon.OutstandingRent > 0 {
		add("collection", in.Recon.OutstandingRent, 2, map[string]any{"outstanding_paise": in.Recon.OutstandingRent},
			"Billed rent uncollected", "Follow up overdue dues")
	}
	if in.LoyaltyBudgetPaise > 0 && in.LoyaltyIssuedPaise > in.LoyaltyBudgetPaise {
		add("loyalty", in.LoyaltyIssuedPaise-in.LoyaltyBudgetPaise, 2,
			map[string]any{"issued": in.LoyaltyIssuedPaise, "budget": in.LoyaltyBudgetPaise},
			"Points issued exceed monthly loyalty budget", "Review award rules vs monthly_budget_paise")
	}
	for slot, prep := range in.MealPrepared {
		exp := in.MealExpected[slot]
		if prep > 0 && exp > 0 && prep > exp {
			excess := prep - exp
			add("food", int64(excess)*5000, 2, map[string]any{"slot": slot, "expected": exp, "prepared": prep},
				"Meal prep exceeds RSVP headcount", "Reduce production buffer; do not cut portion quality")
		}
	}
	if len(in.MeterAnomalies) > 0 {
		add("electricity", 0, 2, map[string]any{"rooms": in.MeterAnomalies},
			"Meter consumption anomaly", "Verify meter; do not accuse occupants")
	}
	if in.RepeatRepairPaise > 0 {
		add("maintenance", in.RepeatRepairPaise, 2, map[string]any{"repeat_repair_paise": in.RepeatRepairPaise},
			"Repeat repairs on same asset", "Compare replacement cost vs continued repair")
	}

	recs, _ := s.Store.ListRecommendations(ctx, in.PropertyID)
	return leaks, recs, nil
}

func COI(monthlyLeakagePaise int64, months int) int64 {
	if months <= 0 {
		months = 6
	}
	return monthlyLeakagePaise * int64(months)
}

func ForecastOCF(current int64, growthBPS int, months int) []int64 {
	out := make([]int64, months)
	v := current
	for i := 0; i < months; i++ {
		v = v + v*int64(growthBPS)/10000
		out[i] = v
	}
	return out
}
