package finance

import (
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func DefaultPolicy(propertyID uuid.UUID) domain.ApprovalPolicy {
	return domain.DefaultPolicy(propertyID)
}

func defaultPolicy(propertyID uuid.UUID) domain.ApprovalPolicy {
	return domain.DefaultPolicy(propertyID)
}

func DefaultSettings(propertyID uuid.UUID) domain.PropertyFinanceSettings {
	return domain.DefaultSettings(propertyID)
}

func defaultSettings(propertyID uuid.UUID) domain.PropertyFinanceSettings {
	return domain.DefaultSettings(propertyID)
}

type SpendCheck = domain.SpendCheck

type spendCheck = domain.SpendCheck

func EvaluateManagerSpend(p domain.ApprovalPolicy, amount, dailySoFar, monthlySoFar int64, emergency bool) SpendCheck {
	return domain.EvaluateManagerSpend(p, amount, dailySoFar, monthlySoFar, emergency)
}

func evaluateManagerSpend(p domain.ApprovalPolicy, amount, dailySoFar, monthlySoFar int64, emergency bool) SpendCheck {
	return domain.EvaluateManagerSpend(p, amount, dailySoFar, monthlySoFar, emergency)
}

func DayStart(t time.Time) time.Time {
	return domain.DayStart(t)
}

func dayStart(t time.Time) time.Time {
	return domain.DayStart(t)
}
