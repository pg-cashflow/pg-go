package domain

import (
	"time"

	"github.com/google/uuid"
)

type ReferralRewardDestination string

const (
	ReferralDestReferrerCredit ReferralRewardDestination = "referrer_credit"
	ReferralDestPassToJoiner   ReferralRewardDestination = "pass_to_joiner"
)

type ReferralDestinationSetBy string

const (
	ReferralSetByOwnerManual ReferralDestinationSetBy = "owner_manual"
	ReferralSetByTenant      ReferralDestinationSetBy = "tenant"
	ReferralSetByReferrerSMS ReferralDestinationSetBy = "referrer_sms"
)

// ReferralAdmission tracks a prospective or bound new joiner (P1 first-rent referral).
type ReferralAdmission struct {
	ID                    uuid.UUID                  `json:"id"`
	PropertyID            uuid.UUID                  `json:"property_id"`
	ReferrerTenantID      *uuid.UUID                 `json:"referrer_tenant_id,omitempty"`
	ReferrerExternalPhone string                     `json:"referrer_external_phone,omitempty"`
	ReferrerExternalName  string                     `json:"referrer_external_name,omitempty"`
	ReferredTenantID      *uuid.UUID                 `json:"referred_tenant_id,omitempty"`
	Phone                 string                     `json:"phone"`
	Name                  string                     `json:"name"`
	ReferralCode          string                     `json:"referral_code,omitempty"`
	JoinerDiscountBps     int                        `json:"joiner_discount_bps"`
	ReferrerRewardBps     int                        `json:"referrer_reward_bps"`
	RewardDestination     *ReferralRewardDestination `json:"reward_destination,omitempty"`
	DestinationSetBy      *ReferralDestinationSetBy  `json:"destination_set_by,omitempty"`
	DestinationSetAt      *time.Time                 `json:"destination_set_at,omitempty"`
	Status                string                     `json:"status"`
	BoundAt               *time.Time                 `json:"bound_at,omitempty"`
	FirstRentDueID        *uuid.UUID                 `json:"first_rent_due_id,omitempty"`
	RewardedPaymentID     *uuid.UUID                 `json:"rewarded_payment_id,omitempty"`
	PointsAwarded         int                        `json:"points_awarded"`
	CreatedAt             time.Time                  `json:"created_at"`
	RewardedAt            *time.Time                 `json:"rewarded_at,omitempty"`
}
