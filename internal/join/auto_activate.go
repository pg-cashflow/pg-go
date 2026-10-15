package join

import (
	"context"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type activateTerms struct {
	RentPaise    int64
	DepositPaise int64
	DueDay       int16
	RoomNumber   *string
	Should       bool
}

func resolveAutoActivateTerms(slot *domain.SlotInvite, settings *domain.PropertyOnboardingSettings, roomNumber string) activateTerms {
	out := activateTerms{}
	if slot != nil {
		out.Should = true
		out.RentPaise = slot.RentPaise
		out.DepositPaise = slot.DepositPaise
		out.DueDay = slot.DueDay
		if roomNumber != "" {
			out.RoomNumber = &roomNumber
		}
		return out
	}
	if settings == nil || !settings.AutoActivate {
		return out
	}
	if settings.DefaultRentPaise <= 0 || settings.DefaultDueDay == nil || *settings.DefaultDueDay < 1 {
		return out
	}
	out.Should = true
	out.RentPaise = settings.DefaultRentPaise
	out.DepositPaise = settings.DefaultDepositPaise
	if out.DepositPaise <= 0 {
		out.DepositPaise = out.RentPaise
	}
	out.DueDay = *settings.DefaultDueDay
	return out
}

func (s *Service) roomNumber(ctx context.Context, roomID uuid.UUID) (string, error) {
	if s.slotDB == nil {
		return "", nil
	}
	return s.slotDB.RoomNumber(ctx, roomID)
}

func profileSchemaVersion(j *domain.JoinRequest, slot *domain.SlotInvite, settings *domain.PropertyOnboardingSettings) int {
	if slot != nil {
		return slot.ProfileSchemaVersion
	}
	if settings != nil && settings.ProfileSchemaVersion > 0 {
		return settings.ProfileSchemaVersion
	}
	if j != nil && j.ProfileSchemaVersion > 0 {
		return j.ProfileSchemaVersion
	}
	return 1
}

// loadSlotAndSettings helpers used from CompleteOnboarding.
func (s *Service) loadOnboardingContext(ctx context.Context, j *domain.JoinRequest) (*domain.SlotInvite, *domain.PropertyOnboardingSettings, error) {
	var slot *domain.SlotInvite
	if j.SlotInviteID != nil && s.slotDB != nil {
		got, err := s.slotDB.GetByID(ctx, *j.SlotInviteID)
		if err != nil {
			return nil, nil, err
		}
		slot = got
	}
	var settings *domain.PropertyOnboardingSettings
	if s.onboardingDB != nil {
		settings, _ = s.onboardingDB.Get(ctx, j.PropertyID)
	}
	return slot, settings, nil
}
