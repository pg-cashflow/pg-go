package domain

import (
	"time"

	"github.com/google/uuid"
)

type SlotInviteStatus string

const (
	SlotInviteOpen     SlotInviteStatus = "open"
	SlotInviteConsumed SlotInviteStatus = "consumed"
	SlotInviteRevoked  SlotInviteStatus = "revoked"
	SlotInviteExpired  SlotInviteStatus = "expired"
)

type SlotInvite struct {
	ID                   uuid.UUID        `json:"id"`
	PropertyID           uuid.UUID        `json:"property_id"`
	FloorID              uuid.UUID        `json:"floor_id"`
	RoomID               uuid.UUID        `json:"room_id"`
	Code                 string           `json:"code"`
	RentPaise            int64            `json:"rent_paise"`
	DepositPaise         int64            `json:"deposit_paise"`
	DueDay               int16            `json:"due_day"`
	Status               SlotInviteStatus `json:"status"`
	TermsFrozenAt        time.Time        `json:"terms_frozen_at"`
	ProfileSchemaVersion int              `json:"profile_schema_version"`
	AllowOverCapacity    bool             `json:"allow_over_capacity"`
	BoundPhone           string           `json:"bound_phone,omitempty"`
	WaitlistEntryID      *uuid.UUID       `json:"waitlist_entry_id,omitempty"`
	ExpiresAt            time.Time        `json:"expires_at"`
	ConsumedAt           *time.Time       `json:"consumed_at,omitempty"`
	ConsumedByUserID     *uuid.UUID       `json:"consumed_by_user_id,omitempty"`
	ConsumedTenantID     *uuid.UUID       `json:"consumed_tenant_id,omitempty"`
	CreatedByUserID      *uuid.UUID       `json:"created_by_user_id,omitempty"`
	CreatedAt            time.Time        `json:"created_at"`
}

type PropertyOnboardingSettings struct {
	PropertyID                  uuid.UUID `json:"property_id"`
	DefaultRentPaise            int64     `json:"default_rent_paise"`
	DefaultDepositPaise         int64     `json:"default_deposit_paise"`
	DefaultDueDay               *int16    `json:"default_due_day,omitempty"`
	AutoActivate                bool      `json:"auto_activate"`
	ReserveBedsForOpenInvites   bool      `json:"reserve_beds_for_open_invites"`
	MaxExtraBedsPerRoom         int       `json:"max_extra_beds_per_room"`
	SlotInviteDefaultTTLDays    int       `json:"slot_invite_default_ttl_days"`
	WaitlistEnabled             bool      `json:"waitlist_enabled"`
	WaitlistOfferTTLHours       int       `json:"waitlist_offer_ttl_hours"`
	ProfileSchemaVersion        int       `json:"profile_schema_version"`
	RequireKYCBeforePay         bool      `json:"require_kyc_before_pay"`
	JoinPendingTTLDays          int       `json:"join_pending_ttl_days"`
	ReferralEnabled             bool      `json:"referral_enabled"`
	JoinerDiscountBps           int       `json:"joiner_discount_bps"`
	ReferrerRewardBps           int       `json:"referrer_reward_bps"`
}

type InviteResolution struct {
	PropertyID   uuid.UUID
	SlotInviteID *uuid.UUID
	SlotInvite   *SlotInvite
}
