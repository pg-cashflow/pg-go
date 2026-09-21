package finance

import (
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func defaultPolicy(propertyID uuid.UUID) domain.ApprovalPolicy {
	return domain.ApprovalPolicy{
		PropertyID:                  propertyID,
		ManagerDailyLimitPaise:      1_000_000,
		SingleExpenseLimitPaise:     500_000,
		ManagerMonthlyLimitPaise:    5_000_000,
		OwnerApprovalThresholdPaise: 500_000,
		ReimbursementThresholdPaise: 250_000,
		EmergencyBypassEnabled:      true,
	}
}

func defaultSettings(propertyID uuid.UUID) domain.PropertyFinanceSettings {
	return domain.PropertyFinanceSettings{
		PropertyID:            propertyID,
		FiscalMonthStartDay:   1,
		ManagerCanViewLeakage: true,
		TDRIsEstimated:        true,
	}
}

type spendCheck struct {
	NeedsApproval bool
	Reject        error
}

func evaluateManagerSpend(p domain.ApprovalPolicy, amount, dailySoFar, monthlySoFar int64, emergency bool) spendCheck {
	if amount <= 0 {
		return spendCheck{Reject: ErrInvalidAmount}
	}
	if dailySoFar+amount > p.ManagerDailyLimitPaise && !(emergency && p.EmergencyBypassEnabled) {
		return spendCheck{Reject: ErrPolicyExceeded}
	}
	if monthlySoFar+amount > p.ManagerMonthlyLimitPaise && !(emergency && p.EmergencyBypassEnabled) {
		return spendCheck{Reject: ErrPolicyExceeded}
	}
	if amount > p.SingleExpenseLimitPaise && !(emergency && p.EmergencyBypassEnabled) {
		if amount > p.OwnerApprovalThresholdPaise {
			return spendCheck{NeedsApproval: true}
		}
		return spendCheck{Reject: ErrPolicyExceeded}
	}
	if amount > p.OwnerApprovalThresholdPaise && !(emergency && p.EmergencyBypassEnabled) {
		return spendCheck{NeedsApproval: true}
	}
	return spendCheck{}
}

func dayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
