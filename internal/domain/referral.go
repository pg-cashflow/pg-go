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
	ID                   uuid.UUID
	PropertyID           uuid.UUID
	ReferrerTenantID     *uuid.UUID
	ReferrerExternalPhone string
	ReferrerExternalName  string
	ReferredTenantID     *uuid.UUID
	Phone                string
	Name                 string
	ReferralCode         string
	JoinerDiscountBps    int
	ReferrerRewardBps    int
	RewardDestination    *ReferralRewardDestination
	DestinationSetBy     *ReferralDestinationSetBy
	DestinationSetAt     *time.Time
	Status               string
	BoundAt              *time.Time
	FirstRentDueID       *uuid.UUID
	RewardedPaymentID    *uuid.UUID
	PointsAwarded        int
	CreatedAt            time.Time
	RewardedAt           *time.Time
}
