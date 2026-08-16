package billing

import (
	"time"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

// NoticeDaysGiven returns whole days since notice was given (0 if never given).
func NoticeDaysGiven(noticeGivenAt *time.Time, asOf time.Time) int {
	if noticeGivenAt == nil {
		return 0
	}
	given := noticeGivenAt.UTC()
	asOf = asOf.UTC()
	if asOf.Before(given) {
		return 0
	}
	return int(asOf.Sub(given).Hours() / 24)
}

// PolicyMet is true when notice days meet or exceed the agreed notice period.
func PolicyMet(noticeDays int, noticePeriodDays int16) bool {
	return noticeDays >= int(noticePeriodDays)
}

// NoticeCompliance computes notice_days_given and policy_met for deposit settlement.
func NoticeCompliance(tenant *domain.Tenant, asOf time.Time) (noticeDays int, policyMet bool) {
	if tenant == nil {
		return 0, false
	}
	noticeDays = NoticeDaysGiven(tenant.NoticeGivenAt, asOf)
	policyMet = PolicyMet(noticeDays, tenant.NoticePeriodDays)
	return noticeDays, policyMet
}

// SuggestedRefundPaise returns original deposit if policy met, otherwise 0.
// Owner may still override the actual refunded amount.
func SuggestedRefundPaise(originalPaise int64, policyMet bool) int64 {
	if !policyMet {
		return 0
	}
	return originalPaise
}
