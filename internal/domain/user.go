package domain

import (
	"time"

	"github.com/google/uuid"
)

type Role string

const (
	RoleOwner  Role = "owner"
	RoleTenant Role = "tenant"
	// RoleManager reserved for Phase 3; schema already allows the string.
	RoleManager Role = "manager"
)

type User struct {
	ID           uuid.UUID  `json:"id"`
	Phone        string     `json:"phone,omitempty"`
	Email        string     `json:"email,omitempty"`
	Role         Role       `json:"role"`
	TenantID     *uuid.UUID `json:"tenant_id,omitempty"`
	PropertyID   *uuid.UUID `json:"property_id,omitempty"`
	FirebaseUID  *string    `json:"-"`
	TokenVersion       int        `json:"-"`
	Locale             string     `json:"locale,omitempty"`
	HasSavedPreference bool       `json:"has_saved_preference"`
	CreatedAt          time.Time  `json:"created_at"`
	LastLoginAt        *time.Time `json:"last_login_at,omitempty"`
}

func (u *User) IsOwner() bool {
	return u != nil && u.Role == RoleOwner
}

func (u *User) IsManager() bool {
	return u != nil && u.Role == RoleManager
}

func (u *User) IsManagerOrOwner() bool {
	return u != nil && (u.Role == RoleOwner || u.Role == RoleManager)
}
