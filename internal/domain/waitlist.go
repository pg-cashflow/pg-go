package domain

import (
	"time"

	"github.com/google/uuid"
)

type WaitlistStatus string

const (
	WaitlistWaiting   WaitlistStatus = "waiting"
	WaitlistOffered   WaitlistStatus = "offered"
	WaitlistAccepted  WaitlistStatus = "accepted"
	WaitlistCancelled WaitlistStatus = "cancelled"
)

type RoomWaitlistEntry struct {
	ID                   uuid.UUID      `json:"id"`
	PropertyID           uuid.UUID      `json:"property_id"`
	RoomID               uuid.UUID      `json:"room_id"`
	UserID               *uuid.UUID     `json:"user_id,omitempty"`
	Phone                string         `json:"phone"`
	Status               WaitlistStatus `json:"status"`
	OfferedSlotInviteID  *uuid.UUID     `json:"offered_slot_invite_id,omitempty"`
	OfferExpiresAt       *time.Time     `json:"offer_expires_at,omitempty"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
}
