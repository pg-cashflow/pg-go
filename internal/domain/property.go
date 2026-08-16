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
	InviteCode  string    `json:"invite_code,omitempty"`
	PaymentMode string    `json:"payment_mode,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

const (
	PaymentModeManual   = "manual"
	PaymentModeCashfree = "cashfree"
)
