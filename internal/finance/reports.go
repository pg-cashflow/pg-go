package finance

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// ClearingDriftReport analyzes the gateway_clearing account for unreconciled or stuck balances.
type ClearingDriftReport struct {
	PropertyID              uuid.UUID `json:"property_id"`
	AsOf                    time.Time `json:"as_of"`
	TotalGrossCollectedPaise int64     `json:"total_gross_collected_paise"`
	TotalGrossSettledPaise  int64     `json:"total_gross_settled_paise"`
	TotalRefundedPaise      int64     `json:"total_refunded_paise"`
	NetUnclearedPaise       int64     `json:"net_uncleared_paise"`
	AgingPast3DaysPaise     int64     `json:"aging_past_3_days_paise"`
	HasDriftBreach          bool      `json:"has_drift_breach"`
}

// DepositReserveReport compares ring-fenced bank balance against total tenant deposit obligations.
type DepositReserveReport struct {
	PropertyID               uuid.UUID `json:"property_id"`
	AsOf                     time.Time `json:"as_of"`
	TotalDepositLiabilityPaise int64    `json:"total_deposit_liability_paise"`
	BankCashBalancePaise     int64     `json:"bank_cash_balance_paise"`
	ReserveSurplusDeficitPaise int64    `json:"reserve_surplus_deficit_paise"`
	IsDeficit                bool      `json:"is_deficit"`
	CoveragePercentageBPS    int       `json:"coverage_percentage_bps"` // 10000 = 100%
}

// CapitalPaybackReport analyzes capital invested versus cumulative operating cash flows.
type CapitalPaybackReport struct {
	PropertyID               uuid.UUID `json:"property_id"`
	AsOf                     time.Time `json:"as_of"`
	TotalInvestedPaise       int64     `json:"total_invested_paise"`
	TotalWithdrawnPaise      int64     `json:"total_withdrawn_paise"`
	NetCapitalDeployedPaise  int64     `json:"net_capital_deployed_paise"`
	CumulativeOCFPaise       int64     `json:"cumulative_ocf_paise"`
	UnrecoveredCapitalPaise  int64     `json:"unrecovered_capital_paise"`
	IsBreakevenAchieved      bool      `json:"is_breakeven_achieved"`
	PaybackPercentageBPS     int       `json:"payback_percentage_bps"`
}

// SubBusinessDays subtracts N business days (skipping Saturday and Sunday) from t in UTC.
func SubBusinessDays(t time.Time, n int) time.Time {
	curr := t
	daysSubtracted := 0
	for daysSubtracted < n {
		curr = curr.AddDate(0, 0, -1)
		weekday := curr.Weekday()
		if weekday != time.Saturday && weekday != time.Sunday {
			daysSubtracted++
		}
	}
	return curr
}

// GetClearingDriftReport generates the T+3 gateway clearing drift analysis.
func (s *Service) GetClearingDriftReport(ctx context.Context, propertyID uuid.UUID) (*ClearingDriftReport, error) {
	now := s.Now()
	// T+3 business days cutoff: subtracts 3 full business days skipping weekends
	t3Cutoff := SubBusinessDays(now, 3)

	// Fetch all journal lines on gateway_clearing
	lines, err := s.Store.ListJournal(ctx, propertyID, time.Time{}, now.Add(24*time.Hour), domain.AcctGatewayClearing)
	if err != nil {
		return nil, fmt.Errorf("list gateway_clearing journal lines: %w", err)
	}

	var totalDr, totalCr, past3DaysDr, past3DaysCr int64
	for _, l := range lines {
		totalDr += l.DebitPaise
		totalCr += l.CreditPaise
		if l.OccurredAt.Before(t3Cutoff) {
			past3DaysDr += l.DebitPaise
			past3DaysCr += l.CreditPaise
		}
	}

	netUncleared := totalDr - totalCr
	agingPast3Days := past3DaysDr - past3DaysCr
	if agingPast3Days < 0 {
		agingPast3Days = 0
	}

	hasBreach := agingPast3Days > 0

	if hasBreach {
		// Publish high-priority leakage/reconciliation event
		s.publish(ctx, propertyID, domain.EvtLeakageDetected, map[string]any{
			"type":              "gateway_clearing_drift",
			"severity":          "high",
			"aging_paise":       agingPast3Days,
			"net_uncleared":     netUncleared,
			"message":           fmt.Sprintf("T+3 gateway clearing drift breach: %d paise older than 3 business days remained unsettled", agingPast3Days),
		})
	}

	return &ClearingDriftReport{
		PropertyID:               propertyID,
		AsOf:                     now,
		TotalGrossCollectedPaise: totalDr,
		TotalGrossSettledPaise:  totalCr,
		NetUnclearedPaise:       netUncleared,
		AgingPast3DaysPaise:     agingPast3Days,
		HasDriftBreach:          hasBreach,
	}, nil
}

// GetDepositReserveReport generates the liquidity report comparing bank funds to deposit liability.
func (s *Service) GetDepositReserveReport(ctx context.Context, propertyID uuid.UUID) (*DepositReserveReport, error) {
	now := s.Now()
	// Deposit liability is a Credit balance on deposit_liability account
	depositLiab, err := s.Store.SumAccountNetCredit(ctx, propertyID, domain.AcctDepositLiability, time.Time{}, now.Add(24*time.Hour))
	if err != nil {
		return nil, fmt.Errorf("sum deposit liability: %w", err)
	}

	// Bank is an asset (Debit balance on bank)
	bankDr, bankCr, err := s.Store.SumAccount(ctx, propertyID, domain.AcctBank, time.Time{}, now.Add(24*time.Hour))
	if err != nil {
		return nil, fmt.Errorf("sum bank account: %w", err)
	}
	bankBalance := bankDr - bankCr

	surplusDeficit := bankBalance - depositLiab
	isDeficit := surplusDeficit < 0

	var coverageBPS int
	if depositLiab > 0 {
		if bankBalance <= 0 {
			coverageBPS = 0
		} else {
			coverageBPS = int((bankBalance * 10000) / depositLiab)
		}
	} else {
		coverageBPS = 10000
	}

	return &DepositReserveReport{
		PropertyID:                 propertyID,
		AsOf:                       now,
		TotalDepositLiabilityPaise: depositLiab,
		BankCashBalancePaise:       bankBalance,
		ReserveSurplusDeficitPaise: surplusDeficit,
		IsDeficit:                  isDeficit,
		CoveragePercentageBPS:      coverageBPS,
	}, nil
}

// GetCapitalPaybackReport computes dynamic breakeven and unrecovered capital from double-entry records.
func (s *Service) GetCapitalPaybackReport(ctx context.Context, propertyID uuid.UUID) (*CapitalPaybackReport, error) {
	now := s.Now()

	invested, withdrawn, err := s.CapitalTotals(ctx, propertyID)
	if err != nil {
		return nil, fmt.Errorf("get capital totals: %w", err)
	}
	netDeployed := invested - withdrawn

	// Cumulative revenue (rent + utility recovery)
	rent, _ := s.Store.SumAccountNetCredit(ctx, propertyID, domain.AcctRentRevenue, time.Time{}, now.Add(24*time.Hour))
	util, _ := s.Store.SumAccountNetCredit(ctx, propertyID, domain.AcctUtilityRecoveryRevenue, time.Time{}, now.Add(24*time.Hour))
	rev := rent + util

	// Cumulative opex (operating_expense + payment_processing_expense + loyalty_expense)
	opexDr, _, _ := s.Store.SumAccount(ctx, propertyID, domain.AcctOperatingExpense, time.Time{}, now.Add(24*time.Hour))
	tdrDr, _, _ := s.Store.SumAccount(ctx, propertyID, domain.AcctPaymentProcessingExpense, time.Time{}, now.Add(24*time.Hour))
	loyaltyDr, _, _ := s.Store.SumAccount(ctx, propertyID, domain.AcctLoyaltyExpense, time.Time{}, now.Add(24*time.Hour))
	opex := opexDr + tdrDr + loyaltyDr

	cumulativeOCF := rev - opex
	unrecovered := netDeployed - cumulativeOCF
	if unrecovered < 0 {
		unrecovered = 0
	}

	isBreakeven := cumulativeOCF >= netDeployed

	var paybackBPS int
	if netDeployed > 0 {
		if cumulativeOCF <= 0 {
			paybackBPS = 0
		} else {
			paybackBPS = int((cumulativeOCF * 10000) / netDeployed)
		}
	} else {
		paybackBPS = 10000
	}

	return &CapitalPaybackReport{
		PropertyID:              propertyID,
		AsOf:                    now,
		TotalInvestedPaise:      invested,
		TotalWithdrawnPaise:     withdrawn,
		NetCapitalDeployedPaise: netDeployed,
		CumulativeOCFPaise:      cumulativeOCF,
		UnrecoveredCapitalPaise: unrecovered,
		IsBreakevenAchieved:     isBreakeven,
		PaybackPercentageBPS:    paybackBPS,
	}, nil
}
