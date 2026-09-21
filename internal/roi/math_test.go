package roi

import (
	"testing"

	"github.com/pg-cashflow/pg-go/internal/finance"
)

func TestComputeBreakEven(t *testing.T) {
	occ := finance.Occupancy{CapacityBeds: 100, OccupiedBeds: 72}
	be := ComputeBreakEven(30_000_000, 72*370_000, 72*1_000_000, occ)
	if be.BreakEvenBeds < 40 || be.BreakEvenBeds > 55 {
		t.Fatalf("break-even beds=%d contrib=%d", be.BreakEvenBeds, be.ContributionPerBedPaise)
	}
	if be.CurrentOccupancyBPS != 7200 {
		t.Fatalf("occ bps=%d", be.CurrentOccupancyBPS)
	}
}

func TestComputeRecovery(t *testing.T) {
	r := ComputeRecovery(2_000_000_00, 0, 8_500_000_0, 1_150_000_0)
	if r.UnrecoveredPaise <= 0 {
		t.Fatalf("%+v", r)
	}
	if r.TBEMonthsMilli == nil {
		t.Fatal("expected TBE")
	}
}

func TestSimulate(t *testing.T) {
	out := Simulate(ScenarioInput{
		OccupancyBPS:     8500,
		RentPerBedPaise:  1_030_000,
		FoodCostPaise:    200_000_00,
		ElectricityPaise: 50_000_00,
		FixedOpexPaise:   300_000_00,
		CapacityBeds:     100,
		CapitalRemaining: 1_150_000_00,
	})
	if out.OCFPaise <= 0 {
		t.Fatalf("ocf=%d", out.OCFPaise)
	}
}
