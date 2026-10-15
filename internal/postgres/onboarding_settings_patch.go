package postgres

import (
	"errors"
	"fmt"
)

var ErrOnboardingSettingsInvalid = errors.New("onboarding: invalid settings")

// OnboardingSettingsPatch carries optional fields for PATCH updates.
type OnboardingSettingsPatch struct {
	DefaultRentPaise          *int64
	DefaultDepositPaise       *int64
	DefaultDueDay             *int16
	AutoActivate              *bool
	ReserveBedsForOpenInvites *bool
	MaxExtraBedsPerRoom       *int
	SlotInviteDefaultTTLDays  *int
	WaitlistEnabled           *bool
	WaitlistOfferTTLHours     *int
	RequireKYCBeforePay       *bool
	JoinPendingTTLDays        *int
	ReferralEnabled           *bool
	JoinerDiscountBps         *int
	ReferrerRewardBps         *int
}

func (p OnboardingSettingsPatch) Validate() error {
	if p.DefaultDueDay != nil {
		if *p.DefaultDueDay < 1 || *p.DefaultDueDay > 28 {
			return fmt.Errorf("%w: due_day", ErrOnboardingSettingsInvalid)
		}
	}
	if p.JoinerDiscountBps != nil && (*p.JoinerDiscountBps < 0 || *p.JoinerDiscountBps > 10000) {
		return fmt.Errorf("%w: joiner_discount_bps", ErrOnboardingSettingsInvalid)
	}
	if p.ReferrerRewardBps != nil && (*p.ReferrerRewardBps < 0 || *p.ReferrerRewardBps > 10000) {
		return fmt.Errorf("%w: referrer_reward_bps", ErrOnboardingSettingsInvalid)
	}
	if p.WaitlistOfferTTLHours != nil && *p.WaitlistOfferTTLHours <= 0 {
		return fmt.Errorf("%w: waitlist_offer_ttl_hours", ErrOnboardingSettingsInvalid)
	}
	if p.SlotInviteDefaultTTLDays != nil && *p.SlotInviteDefaultTTLDays <= 0 {
		return fmt.Errorf("%w: slot_invite_default_ttl_days", ErrOnboardingSettingsInvalid)
	}
	if p.JoinPendingTTLDays != nil && *p.JoinPendingTTLDays <= 0 {
		return fmt.Errorf("%w: join_pending_ttl_days", ErrOnboardingSettingsInvalid)
	}
	if p.MaxExtraBedsPerRoom != nil && *p.MaxExtraBedsPerRoom < 0 {
		return fmt.Errorf("%w: max_extra_beds_per_room", ErrOnboardingSettingsInvalid)
	}
	return nil
}
