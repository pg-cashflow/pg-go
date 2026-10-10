package roi

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

type Service struct {
	Finance *finance.Service
	Now     func() time.Time
}

func NewService(fin *finance.Service) *Service {
	return &Service{Finance: fin, Now: func() time.Time { return time.Now().UTC() }}
}

type Report struct {
	Period    string                    `json:"period"`
	Occupancy finance.Occupancy         `json:"occupancy"`
	Operating *finance.OperatingSummary `json:"operating"`
	BreakEven BreakEven                 `json:"break_even"`
	Recovery  Recovery                  `json:"recovery"`
	Official  bool                      `json:"official"`
}

func (s *Service) Report(ctx context.Context, propertyID uuid.UUID, period string, recon *payment.ReconciliationSummary, rooms []domain.Room, tenants []domain.Tenant, billedRent, fixedOpex, variableOpex int64) (*Report, error) {
	occ := finance.ComputeOccupancy(rooms, tenants, s.Now())
	op, err := s.Finance.OperatingSummary(ctx, propertyID, period, recon.RentCollected)
	if err != nil {
		return nil, err
	}
	if billedRent == 0 {
		billedRent = op.OperatingRevenuePaise
	}
	if fixedOpex == 0 && variableOpex == 0 {
		variableOpex = op.OpexPaise / 2
		fixedOpex = op.OpexPaise - variableOpex
	}
	be := ComputeBreakEven(fixedOpex, variableOpex, op.OperatingRevenuePaise, occ)
	invested, withdrawn, _ := s.Finance.CapitalTotals(ctx, propertyID)
	rec := ComputeRecovery(invested, withdrawn, op.OCFPaise, op.OCFPaise)
	official := false
	if t, err := s.Finance.Store.GetTieOut(ctx, propertyID, period); err == nil && t.Status == "closed" {
		official = true
	}
	return &Report{
		Period:    period,
		Occupancy: occ,
		Operating: op,
		BreakEven: be,
		Recovery:  rec,
		Official:  official,
	}, nil
}

func (s *Service) PersistSnapshots(ctx context.Context, propertyID uuid.UUID, period string, r *Report) error {
	day := s.Now()
	kpi := &domain.KPISnapshot{
		PropertyID:              propertyID,
		PeriodMonth:             period,
		SnapshotDate:            day,
		OccupancyBPS:            finance.OccupancyBPS(r.Occupancy),
		OccupiedBeds:            r.Occupancy.OccupiedBeds,
		CapacityBeds:            r.Occupancy.CapacityBeds,
		ContributionPerBedPaise: r.BreakEven.ContributionPerBedPaise,
		OpexPaise:               r.Operating.OpexPaise,
		OCFPaise:                r.Operating.OCFPaise,
	}
	if err := s.Finance.Store.InsertKPI(ctx, kpi); err != nil {
		return err
	}
	roi := &domain.ROISnapshot{
		PropertyID:            propertyID,
		PeriodMonth:           period,
		SnapshotDate:          day,
		CapitalInvestedPaise:  r.Recovery.CapitalInvestedPaise,
		CapitalRecoveredPaise: r.Recovery.CapitalRecoveredPaise,
		UnrecoveredPaise:      r.Recovery.UnrecoveredPaise,
		TBEMonthsMilli:        r.Recovery.TBEMonthsMilli,
		Official:              r.Official,
	}
	if r.BreakEven.BreakEvenOccupancyBPS > 0 {
		bps := r.BreakEven.BreakEvenOccupancyBPS
		roi.BreakEvenOccupancyBPS = &bps
	}
	return s.Finance.Store.InsertROI(ctx, roi)
}
