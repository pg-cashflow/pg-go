package finance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

// CriticalTieOutVarianceThresholdPaise defines the magnitude (₹5,000 / 500,000 paise)
// where any single period's unreconciled variance immediately escalates to the operator.
const CriticalTieOutVarianceThresholdPaise int64 = 500_000 // ₹5,000.00

// ComputeTieOut compares collections control (reconciliation) to ledger rent_revenue credits.
func (s *Service) ComputeTieOut(ctx context.Context, propertyID uuid.UUID, period string, recon *payment.ReconciliationSummary) (*domain.PeriodTieOut, error) {
	from, to, err := PeriodBounds(period)
	if err != nil {
		return nil, err
	}
	// A closed period is frozen (DB control C-3): return the closing snapshot untouched.
	// Recomputing would be rejected by the database and, more importantly, would let a
	// read endpoint rewrite an audited close.
	existing, err := s.Store.GetTieOut(ctx, propertyID, period)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if existing != nil && existing.Status == "closed" {
		return existing, nil
	}
	ledgerRent, err := s.Store.SumAccountNetCredit(ctx, propertyID, domain.AcctRentRevenue, from, to)
	if err != nil {
		return nil, err
	}
	diff := recon.RentCollected - ledgerRent
	items := []domain.TieOutItem{}
	if diff != 0 {
		items = append(items, domain.TieOutItem{
			Category:         "investigate",
			AmountPaise:      diff,
			Note:             "unexplained collections vs ledger rent_revenue",
			UnresolvedMonths: 1,
		})
	}
	t := &domain.PeriodTieOut{
		PropertyID:       propertyID,
		PeriodMonth:      period,
		ReconTotalPaise:  recon.RentCollected,
		LedgerTotalPaise: ledgerRent,
		DifferencePaise:  diff,
		Items:            items,
		Status:           "open",
	}
	if prevPeriod, err := previousPeriodOf(period); err == nil {
		if prev, err := s.Store.GetTieOut(ctx, propertyID, prevPeriod); err == nil && prev != nil {
			t.Items = mergeInvestigateAging(prev.Items, items)
		}
	}
	if existing != nil {
		t.ID = existing.ID
		t.Status = existing.Status
		t.ClosedAt = existing.ClosedAt
	}
	if err := s.Store.SaveTieOut(ctx, t); err != nil {
		return nil, err
	}
	absDiff := diff
	if absDiff < 0 {
		absDiff = -absDiff
	}
	if absDiff >= CriticalTieOutVarianceThresholdPaise {
		s.publish(ctx, propertyID, domain.EvtCriticalTieOutVariance, t)
	}
	if aging := maxInvestigateMonths(t.Items); aging >= 2 {
		s.publish(ctx, propertyID, domain.EvtRecurringTieOutException, t)
	}
	return t, nil
}

func mergeInvestigateAging(prev, next []domain.TieOutItem) []domain.TieOutItem {
	had := false
	months := 1
	for _, p := range prev {
		if p.Category == "investigate" && p.AmountPaise != 0 {
			had = true
			if p.UnresolvedMonths > 0 {
				months = p.UnresolvedMonths + 1
			} else {
				months = 2
			}
		}
	}
	out := next
	if !had {
		return out
	}
	for i := range out {
		if out[i].Category == "investigate" {
			out[i].UnresolvedMonths = months
		}
	}
	return out
}

func maxInvestigateMonths(items []domain.TieOutItem) int {
	m := 0
	for _, i := range items {
		if i.UnresolvedMonths > m {
			m = i.UnresolvedMonths
		}
	}
	return m
}

func (s *Service) CloseTieOut(ctx context.Context, propertyID uuid.UUID, period string) (*domain.PeriodTieOut, error) {
	from, periodEnd, err := PeriodBounds(period)
	if err != nil {
		return nil, err
	}
	now := s.Now()
	if now.Before(periodEnd) {
		return nil, fmt.Errorf("%w: period %s has not ended", ErrPeriodNotCloseable, period)
	}

	existing, err := s.Store.GetTieOut(ctx, propertyID, period)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if existing != nil && existing.Status == "closed" {
		return existing, nil
	}

	ledgerRent, err := s.Store.SumAccountNetCredit(ctx, propertyID, domain.AcctRentRevenue, from, periodEnd)
	if err != nil {
		return nil, err
	}
	reconCollected := int64(0)
	if existing != nil {
		reconCollected = existing.ReconTotalPaise
	}
	diff := reconCollected - ledgerRent
	if diff != 0 {
		t := &domain.PeriodTieOut{
			PropertyID:       propertyID,
			PeriodMonth:      period,
			ReconTotalPaise:  reconCollected,
			LedgerTotalPaise: ledgerRent,
			DifferencePaise:  diff,
			Status:           "open",
		}
		s.publish(ctx, propertyID, domain.EvtPeriodTieOutBlocked, t)
		return t, ErrPeriodNotCloseable
	}

	t := &domain.PeriodTieOut{
		PropertyID:       propertyID,
		PeriodMonth:      period,
		ReconTotalPaise:  reconCollected,
		LedgerTotalPaise: ledgerRent,
		DifferencePaise:  0,
		Status:           "closed",
		ClosedAt:         &now,
	}
	if existing != nil {
		t.ID = existing.ID
		t.Items = existing.Items
	}
	for _, item := range t.Items {
		if item.Category == "investigate" && item.AmountPaise != 0 {
			s.publish(ctx, propertyID, domain.EvtPeriodTieOutBlocked, t)
			return t, fmt.Errorf("%w: unresolved investigation items exist", ErrPeriodNotCloseable)
		}
	}
	if err := s.Store.SaveTieOut(ctx, t); err != nil {
		return nil, err
	}
	return t, nil
}

// ReopenTieOut reopens a previously closed period with an audited actor and reason.
// Trigger C-3 in PostgreSQL audits this action immutably in ledger_control_overrides.
func (s *Service) ReopenTieOut(ctx context.Context, propertyID uuid.UUID, period, actor, reason string) (*domain.PeriodTieOut, error) {
	if reason == "" {
		return nil, fmt.Errorf("%w: reason is required", ErrPeriodNotReopenable)
	}
	t, err := s.Store.GetTieOut(ctx, propertyID, period)
	if err != nil {
		return nil, err
	}
	if t.Status != "closed" {
		return t, ErrPeriodNotReopenable
	}
	if err := s.Store.ReopenTieOut(ctx, propertyID, period, actor); err != nil {
		return nil, err
	}
	t.Status = "open"
	t.ClosedAt = nil
	s.publish(ctx, propertyID, domain.EvtTieOutReopened, map[string]any{
		"period": period,
		"actor":  actor,
		"reason": reason,
	})
	return t, nil
}

func (s *Service) RecurringTieOutAlert(ctx context.Context, propertyID uuid.UUID) bool {
	list, err := s.Store.ListTieOuts(ctx, propertyID, 12)
	if err != nil {
		return false
	}
	n := 0
	for _, t := range list {
		if t.DifferencePaise != 0 {
			n++
		}
	}
	return n >= 2
}

func previousPeriodOf(period string) (string, error) {
	t, err := time.Parse("2006-01", period)
	if err != nil {
		return "", err
	}
	return t.AddDate(0, -1, 0).Format("2006-01"), nil
}
