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
	ID          uuid.UUID  `json:"id"`
	Phone       string     `json:"phone"`
	Role        Role       `json:"role"`
	TenantID    *uuid.UUID `json:"tenant_id,omitempty"`
	PropertyID  *uuid.UUID `json:"property_id,omitempty"`
	FirebaseUID *string    `json:"-"`
	CreatedAt   time.Time  `json:"created_at"`
	LastLoginAt *time.Time `json:"last_login_at,omitempty"`
}
