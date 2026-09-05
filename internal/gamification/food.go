package gamification

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type HeadcountReport struct {
	MealDate  string `json:"meal_date"`
	Breakfast int    `json:"breakfast"`
	Lunch     int    `json:"lunch"`
	Dinner    int    `json:"dinner"`
	Total     int    `json:"total"`
}

// SubmitMealRSVP records tenant meal attendance, enforcing the 8:00 PM previous day cutoff.
func (s *Service) SubmitMealRSVP(ctx context.Context, tenantID uuid.UUID, mealDate time.Time, slot string, attending bool) (*domain.MealRSVP, error) {
	tenant, err := s.tenants.GetByID(ctx, tenantID)
	if err != nil {
		return nil, ErrTenantNotFound
	}

	// Verify cutoff time: 8:00 PM (20:00) the previous day
	now := s.now().UTC()
	// Cutoff is 8:00 PM on mealDate - 1 day
	cutoff := time.Date(mealDate.Year(), mealDate.Month(), mealDate.Day()-1, 20, 0, 0, 0, time.UTC)
	if now.After(cutoff) {
		return nil, ErrRSVPCutoffPassed
	}

	rsvp := &domain.MealRSVP{
		TenantID:   tenantID,
		PropertyID: tenant.PropertyID,
		MealDate:   time.Date(mealDate.Year(), mealDate.Month(), mealDate.Day(), 0, 0, 0, 0, time.UTC),
		MealSlot:   slot,
		Attending:  attending,
	}

	if err := s.store.UpsertMealRSVP(ctx, rsvp); err != nil {
		return nil, err
	}

	// Award 2 points for on-time RSVP (under isolated RSVP sub-cap)
	refType := "rsvp"
	refID := fmt.Sprintf("%s-%s", rsvp.MealDate.Format("20060102"), slot)
	_, _ = s.AwardPoints(ctx, tenantID, "MEAL_RSVP_ON_TIME", &refType, &refID, nil)

	_ = s.pub.Publish(ctx, domain.Event{
		TenantID:   &tenantID,
		PropertyID: tenant.PropertyID,
		EventType:  domain.EvtMealRSVPConfirmed,
		OccurredAt: s.now().UTC(),
	})

	return rsvp, nil
}

// GetHeadcountReport computes kitchen prep headcount for a given date.
func (s *Service) GetHeadcountReport(ctx context.Context, propertyID uuid.UUID, date time.Time) (*HeadcountReport, error) {
	rsvps, err := s.store.ListMealRSVPsByDate(ctx, propertyID, date)
	if err != nil {
		return nil, err
	}

	rep := &HeadcountReport{
		MealDate: date.Format("2006-01-02"),
	}

	for _, r := range rsvps {
		if !r.Attending {
			continue
		}
		switch r.MealSlot {
		case "breakfast":
			rep.Breakfast++
		case "lunch":
			rep.Lunch++
		case "dinner":
			rep.Dinner++
		}
	}
	rep.Total = rep.Breakfast + rep.Lunch + rep.Dinner
	return rep, nil
}
