package finance

import (
	"errors"
	"time"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrDuplicateIdempotency = domain.ErrDuplicateIdempotency
	ErrNotFound             = domain.ErrNotFound
	ErrPeriodClosed         = domain.ErrPeriodClosed
	ErrForbidden            = domain.ErrForbidden
	ErrPolicyExceeded       = errors.New("finance: spend policy exceeded")
	ErrApprovalRequired     = errors.New("finance: owner approval required")
	ErrExpenseNotPayable    = domain.ErrExpenseNotPayable
	ErrOverpay              = domain.ErrOverpay
	ErrUnbalancedJournal    = errors.New("finance: journal lines do not balance")
	ErrPeriodNotCloseable   = errors.New("finance: unexplained tie-out difference blocks close")
	ErrPeriodNotReopenable  = domain.ErrPeriodNotReopenable
	ErrIdempotencyRequired  = errors.New("finance: Idempotency-Key required")
	ErrInvalidAmount        = errors.New("finance: amount must be positive")
	ErrInvalidKind          = errors.New("finance: invalid kind")
	ErrDisabled             = errors.New("finance: finance layer disabled")
)

type Occupancy struct {
	CapacityBeds int
	OccupiedBeds int
	BedsAtRisk   int
}

func OccupancyBPS(o Occupancy) int {
	if o.CapacityBeds <= 0 {
		return 0
	}
	return o.OccupiedBeds * 10000 / o.CapacityBeds
}

func ComputeOccupancy(rooms []domain.Room, tenants []domain.Tenant, now time.Time) Occupancy {
	var o Occupancy
	for _, r := range rooms {
		o.CapacityBeds += int(r.Capacity)
	}
	for _, t := range tenants {
		if t.Status != domain.TenantStatusActive {
			continue
		}
		o.OccupiedBeds++
		if t.NoticeGivenAt != nil {
			end := t.NoticeGivenAt.AddDate(0, 0, int(t.NoticePeriodDays))
			if !end.Before(now) {
				o.BedsAtRisk++
			}
		}
	}
	return o
}

func PeriodBounds(period string) (from, to time.Time, err error) {
	t, err := time.Parse("2006-01", period)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	from = time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	to = from.AddDate(0, 1, 0)
	return from, to, nil
}
