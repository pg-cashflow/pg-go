package finance

import (
	"context"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

type VarianceInput struct {
	Period            string
	BudgetedOCFPaise  int64
	BudgetOccupancyBPS int
	BudgetRentPerBed  int64
	CapacityBeds      int
	OccupiedBeds      int
	BilledRentPaise   int64
	CollectedRentPaise int64
	CategoryOpex      map[string]int64
	BudgetOpex        map[string]int64
	TDRPaise          int64
	TDREstimated      bool
	LoyaltyVariance   int64
}

func BuildVarianceBridge(in VarianceInput, actualOCF int64) domain.VarianceBridge {
	lines := []domain.VarianceLine{}
	if in.CapacityBeds > 0 && in.BudgetRentPerBed > 0 {
		budgetBeds := in.CapacityBeds * in.BudgetOccupancyBPS / 10000
		vol := int64(in.OccupiedBeds-budgetBeds) * in.BudgetRentPerBed
		if vol != 0 {
			lines = append(lines, domain.VarianceLine{Driver: "occupancy_volume", AmountPaise: vol, Kind: "actual"})
		}
		if in.OccupiedBeds > 0 {
			actualRate := in.BilledRentPaise / int64(in.OccupiedBeds)
			rate := (actualRate - in.BudgetRentPerBed) * int64(in.OccupiedBeds)
			if rate != 0 {
				lines = append(lines, domain.VarianceLine{Driver: "rate_rent_per_bed", AmountPaise: rate, Kind: "actual"})
			}
		}
	}
	coll := in.CollectedRentPaise - in.BilledRentPaise
	if coll != 0 {
		lines = append(lines, domain.VarianceLine{Driver: "collection", AmountPaise: coll, Kind: "actual"})
	}
	for _, cat := range []string{"food", "electricity"} {
		act := in.CategoryOpex[cat]
		bud := in.BudgetOpex[cat]
		if act != 0 || bud != 0 {
			lines = append(lines, domain.VarianceLine{Driver: cat + "_cost", AmountPaise: bud - act, Kind: "actual"})
		}
	}
	if in.LoyaltyVariance != 0 {
		lines = append(lines, domain.VarianceLine{Driver: "reward_liability", AmountPaise: -in.LoyaltyVariance, Kind: "actual"})
	}
	if in.TDRPaise != 0 {
		kind := "actual"
		if in.TDREstimated {
			kind = "estimated"
		}
		lines = append(lines, domain.VarianceLine{Driver: "payment_processing_tdr", AmountPaise: -in.TDRPaise, Kind: kind})
	}
	var sum int64
	for _, l := range lines {
		sum += l.AmountPaise
	}
	residual := actualOCF - (in.BudgetedOCFPaise + sum)
	return domain.VarianceBridge{
		PeriodMonth:      in.Period,
		BudgetedOCFPaise: in.BudgetedOCFPaise,
		ActualOCFPaise:   actualOCF,
		Lines:            lines,
		ResidualPaise:    residual,
	}
}

func (s *Service) VarianceBridge(ctx context.Context, propertyID uuid.UUID, period string, recon *payment.ReconciliationSummary, occ Occupancy, billedRent int64) (*domain.VarianceBridge, error) {
	from, to, err := PeriodBounds(period)
	if err != nil {
		return nil, err
	}
	sum, err := s.OperatingSummary(ctx, propertyID, period, recon.RentCollected)
	if err != nil {
		return nil, err
	}
	cats, _ := s.CategoryOpex(ctx, propertyID, from, to)
	buds, _ := s.Store.ListBudgets(ctx, propertyID, period)
	budMap := map[string]int64{}
	var budgetOCF int64
	for _, b := range buds {
		if b.CategoryCode == "ocf" {
			budgetOCF = b.AmountPaise
			continue
		}
		budMap[b.CategoryCode] = b.AmountPaise
	}
	loyalty, _ := s.Store.SumRewardLiability(ctx, propertyID, "issued", from, to)
	st, _ := s.Store.GetSettings(ctx, propertyID)
	rentPerBed := int64(0)
	if occ.OccupiedBeds > 0 {
		rentPerBed = billedRent / int64(occ.OccupiedBeds)
	}
	in := VarianceInput{
		Period:             period,
		BudgetedOCFPaise:   budgetOCF,
		BudgetOccupancyBPS: 8000,
		BudgetRentPerBed:   rentPerBed,
		CapacityBeds:       occ.CapacityBeds,
		OccupiedBeds:       occ.OccupiedBeds,
		BilledRentPaise:    billedRent,
		CollectedRentPaise: recon.RentCollected,
		CategoryOpex:       cats,
		BudgetOpex:         budMap,
		TDRPaise:           sum.TDRExpensePaise,
		TDREstimated:       st.TDRIsEstimated,
		LoyaltyVariance:    loyalty,
	}
	br := BuildVarianceBridge(in, sum.OCFPaise)
	return &br, nil
}
