package domain

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrPolicyExceeded = errors.New("finance: spend policy exceeded")
	ErrInvalidAmount  = errors.New("finance: invalid amount")
)

func DefaultPolicy(propertyID uuid.UUID) ApprovalPolicy {
	return ApprovalPolicy{
		PropertyID:                  propertyID,
		ManagerDailyLimitPaise:      1_000_000,
		SingleExpenseLimitPaise:     500_000,
		ManagerMonthlyLimitPaise:    5_000_000,
		OwnerApprovalThresholdPaise: 500_000,
		ReimbursementThresholdPaise: 250_000,
		EmergencyBypassEnabled:      true,
	}
}

func DefaultSettings(propertyID uuid.UUID) PropertyFinanceSettings {
	return PropertyFinanceSettings{
		PropertyID:            propertyID,
		FiscalMonthStartDay:   1,
		ManagerCanViewLeakage: true,
		TDRIsEstimated:        true,
	}
}

type SpendCheck struct {
	NeedsApproval bool
	Reject        error
}

func EvaluateManagerSpend(p ApprovalPolicy, amount, dailySoFar, monthlySoFar int64, emergency bool) SpendCheck {
	if amount <= 0 {
		return SpendCheck{Reject: ErrInvalidAmount}
	}
	if dailySoFar+amount > p.ManagerDailyLimitPaise && !(emergency && p.EmergencyBypassEnabled) {
		return SpendCheck{Reject: ErrPolicyExceeded}
	}
	if monthlySoFar+amount > p.ManagerMonthlyLimitPaise && !(emergency && p.EmergencyBypassEnabled) {
		return SpendCheck{Reject: ErrPolicyExceeded}
	}
	if amount > p.SingleExpenseLimitPaise && !(emergency && p.EmergencyBypassEnabled) {
		if amount > p.OwnerApprovalThresholdPaise {
			return SpendCheck{NeedsApproval: true}
		}
		return SpendCheck{Reject: ErrPolicyExceeded}
	}
	if amount > p.OwnerApprovalThresholdPaise && !(emergency && p.EmergencyBypassEnabled) {
		return SpendCheck{NeedsApproval: true}
	}
	return SpendCheck{}
}

func DayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func PeriodBounds(period string) (from, to time.Time, err error) {
	if len(period) == 7 && period[4] == '-' {
		t, err := time.Parse("2006-01", period)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		from = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
		to = from.AddDate(0, 1, 0)
		return from, to, nil
	}
	if len(period) == 4 {
		t, err := time.Parse("2006", period)
		if err != nil {
			return time.Time{}, time.Time{}, err
		}
		from = time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
		to = from.AddDate(1, 0, 0)
		return from, to, nil
	}
	return time.Time{}, time.Time{}, errors.New("invalid period format")
}
