package roi

import (
	"math"

	"github.com/pg-cashflow/pg-go/internal/finance"
)

type BreakEven struct {
	ContributionPerBedPaise int64   `json:"contribution_per_bed_paise"`
	FixedOpexPaise          int64   `json:"fixed_opex_paise"`
	BreakEvenBeds           int     `json:"break_even_beds"`
	BreakEvenOccupancyBPS   int     `json:"break_even_occupancy_bps"`
	CurrentOccupancyBPS     int     `json:"current_occupancy_bps"`
	SafetyMarginPP          int     `json:"safety_margin_pp"` // percentage points * 100? use occupancy bps delta / 100
	SafetyMarginBPS         int     `json:"safety_margin_bps"`
}

func ComputeBreakEven(fixedOpex, variableOpex, operatingRevenue int64, occ finance.Occupancy) BreakEven {
	var contrib int64
	if occ.OccupiedBeds > 0 {
		varPer := variableOpex / int64(occ.OccupiedBeds)
		revPer := operatingRevenue / int64(occ.OccupiedBeds)
		contrib = revPer - varPer
	}
	be := BreakEven{
		ContributionPerBedPaise: contrib,
		FixedOpexPaise:          fixedOpex,
		CurrentOccupancyBPS:     finance.OccupancyBPS(occ),
	}
	if contrib > 0 && occ.CapacityBeds > 0 {
		beds := int(math.Ceil(float64(fixedOpex) / float64(contrib)))
		be.BreakEvenBeds = beds
		be.BreakEvenOccupancyBPS = beds * 10000 / occ.CapacityBeds
		be.SafetyMarginBPS = be.CurrentOccupancyBPS - be.BreakEvenOccupancyBPS
		be.SafetyMarginPP = be.SafetyMarginBPS / 100
	}
	return be
}

type Recovery struct {
	CapitalInvestedPaise  int64   `json:"capital_invested_paise"`
	CapitalRecoveredPaise int64   `json:"capital_recovered_paise"`
	UnrecoveredPaise      int64   `json:"unrecovered_paise"`
	MonthlyFCFPaise       int64   `json:"monthly_fcf_paise"`
	TBEMonths             float64 `json:"tbe_months"`
	TBEMonthsMilli        *int    `json:"tbe_months_milli,omitempty"`
}

func ComputeRecovery(invested, withdrawn, cumulativeFCF, monthlyFCF int64) Recovery {
	recovered := cumulativeFCF + withdrawn
	if recovered < 0 {
		recovered = 0
	}
	unrec := invested - recovered
	if unrec < 0 {
		unrec = 0
	}
	r := Recovery{
		CapitalInvestedPaise:  invested,
		CapitalRecoveredPaise: recovered,
		UnrecoveredPaise:      unrec,
		MonthlyFCFPaise:       monthlyFCF,
	}
	if monthlyFCF > 0 && unrec > 0 {
		months := float64(unrec) / float64(monthlyFCF)
		r.TBEMonths = months
		ms := int(months * 1000)
		r.TBEMonthsMilli = &ms
	}
	return r
}

type ScenarioInput struct {
	OccupancyBPS     int   `json:"occupancy_bps"`
	RentPerBedPaise  int64 `json:"rent_per_bed_paise"`
	FoodCostPaise    int64 `json:"food_cost_paise"`
	ElectricityPaise int64 `json:"electricity_paise"`
	FixedOpexPaise   int64 `json:"fixed_opex_paise"`
	CapacityBeds     int   `json:"capacity_beds"`
	CapitalRemaining int64 `json:"capital_remaining_paise"`
}

type ScenarioResult struct {
	OccupiedBeds            int     `json:"occupied_beds"`
	RevenuePaise            int64   `json:"revenue_paise"`
	OpexPaise               int64   `json:"opex_paise"`
	OCFPaise                int64   `json:"ocf_paise"`
	BreakEvenOccupancyBPS   int     `json:"break_even_occupancy_bps"`
	TBEMonths               float64 `json:"tbe_months"`
}

func Simulate(in ScenarioInput) ScenarioResult {
	occBeds := 0
	if in.CapacityBeds > 0 {
		occBeds = in.CapacityBeds * in.OccupancyBPS / 10000
	}
	rev := int64(occBeds) * in.RentPerBedPaise
	opex := in.FixedOpexPaise + in.FoodCostPaise + in.ElectricityPaise
	ocf := rev - opex
	var beBPS int
	contrib := int64(0)
	if occBeds > 0 {
		contrib = (rev - in.FoodCostPaise - in.ElectricityPaise) / int64(occBeds)
	}
	if contrib > 0 && in.CapacityBeds > 0 {
		beds := int(math.Ceil(float64(in.FixedOpexPaise) / float64(contrib)))
		beBPS = beds * 10000 / in.CapacityBeds
	}
	var tbe float64
	if ocf > 0 && in.CapitalRemaining > 0 {
		tbe = float64(in.CapitalRemaining) / float64(ocf)
	}
	return ScenarioResult{
		OccupiedBeds:          occBeds,
		RevenuePaise:          rev,
		OpexPaise:             opex,
		OCFPaise:              ocf,
		BreakEvenOccupancyBPS: beBPS,
		TBEMonths:             tbe,
	}
}
