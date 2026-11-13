package postgres

import (
	"encoding/json"
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
	AllowManagerRefunds       *bool
	// ManagerRefundLimitPaise distinguishes "field absent" (Set=false), "clear the limit"
	// (Set=true, Value=nil) and "set a limit" (Set=true, Value!=nil).
	ManagerRefundLimitPaise NullableInt64Patch
}

// NullableInt64Patch is a tri-state PATCH value. JSON null clears, a number sets, absence leaves as is.
type NullableInt64Patch struct {
	Set   bool
	Value *int64
}

// UnmarshalJSON is called only when the key is present, so any call means Set=true.
func (n *NullableInt64Patch) UnmarshalJSON(b []byte) error {
	n.Set = true
	if string(b) == "null" {
		n.Value = nil
		return nil
	}
	var v int64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n.Value = &v
	return nil
}

func (p OnboardingSettingsPatch) Validate() error {
	if p.DefaultDueDay != nil {
		if *p.DefaultDueDay < 1 || *p.DefaultDueDay > 28 {
			return fmt.Errorf("%w: due_day", ErrOnboardingSettingsInvalid)
		}
	}
	if p.RequireKYCBeforePay != nil && !*p.RequireKYCBeforePay {
		return fmt.Errorf("%w: require_kyc_before_pay cannot be disabled; KYC is mandatory platform-wide", ErrOnboardingSettingsInvalid)
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
	if p.ManagerRefundLimitPaise.Set && p.ManagerRefundLimitPaise.Value != nil && *p.ManagerRefundLimitPaise.Value < 0 {
		return fmt.Errorf("%w: manager_refund_limit_paise", ErrOnboardingSettingsInvalid)
	}
	if p.MaxExtraBedsPerRoom != nil && *p.MaxExtraBedsPerRoom < 0 {
		return fmt.Errorf("%w: max_extra_beds_per_room", ErrOnboardingSettingsInvalid)
	}
	return nil
}
