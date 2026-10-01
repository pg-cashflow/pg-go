package domain

import (
	"time"

	"github.com/google/uuid"
)

type Property struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Address     *string   `json:"address,omitempty"`
	OwnerPhone  string    `json:"owner_phone"`
	UPIVPA      string    `json:"-"` // server-side only; never in list responses
	OwnerName   string    `json:"owner_name"`
	OwnerEmail  string    `json:"owner_email"`
	InviteCode            string     `json:"invite_code,omitempty"`
	PaymentMode           string     `json:"payment_mode,omitempty"`
	PaymentCollectionMode string     `json:"payment_collection_mode,omitempty"`
	GatewayEnabledAt      *time.Time `json:"gateway_enabled_at,omitempty"`
	GatewaySubMerchantID  *string    `json:"gateway_sub_merchant_id,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
}

const (
	PaymentModeManual   = "manual"
	PaymentModeCashfree = "cashfree"

	CollectionModeManualProof = "manual_proof"
	CollectionModeGateway     = "gateway"
)

type PropertySettings struct {
	PropertyID          uuid.UUID       `json:"property_id"`
	PayoutAutoDispatch  bool            `json:"payout_auto_dispatch"`
	ReminderOffsets     []int           `json:"reminder_offsets"`
	ReminderCatchUpDays int             `json:"reminder_catch_up_days"`
	ActiveModules       map[string]bool `json:"active_modules"`
	AutoApplyCredit     bool            `json:"auto_apply_credit"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
}

func DefaultPropertySettings(propertyID uuid.UUID) PropertySettings {
	return PropertySettings{
		PropertyID:          propertyID,
		PayoutAutoDispatch:  false,
		ReminderOffsets:     []int{-3, 0, 1, 7},
		ReminderCatchUpDays: 2,
		ActiveModules: map[string]bool{
			"gamification": true,
			"kyc":          true,
			"payouts":      true,
			"accounting":   true,
			"calendar":     true,
		},
		AutoApplyCredit: true,
	}
}
