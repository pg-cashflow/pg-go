package domain

import (
	"time"

	"github.com/google/uuid"
)

type Property struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	Address    *string   `json:"address,omitempty"`
	OwnerPhone string    `json:"owner_phone"`
	UPIVPA     string    `json:"-"` // server-side only; never in API responses
	OwnerName  string    `json:"owner_name"`
	OwnerEmail string    `json:"owner_email"`
	CreatedAt  time.Time `json:"created_at"`
}
