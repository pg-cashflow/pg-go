package domain

import (
	"time"

	"github.com/google/uuid"
)

type UserPreferences struct {
	UserID    uuid.UUID `json:"user_id"`
	Locale    string    `json:"locale"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
