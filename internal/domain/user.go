package domain

import (
	"time"

	"github.com/google/uuid"
)

type Role string

const (
	RoleOwner  Role = "owner"
	RoleTenant Role = "tenant"
	// RoleManager operates daily property management and operational flows.
	RoleManager Role = "manager"
	// RolePlatformOps operates platform-level reconciliation and exception queues.
	RolePlatformOps Role = "platform_ops"
)

type User struct {
	ID                 uuid.UUID  `json:"id"`
	Phone              string     `json:"phone,omitempty"`
	Email              string     `json:"email,omitempty"`
	Role               Role       `json:"role"`
	TenantID           *uuid.UUID `json:"tenant_id,omitempty"`
	PropertyID         *uuid.UUID `json:"property_id,omitempty"`
	FirebaseUID        *string    `json:"-"`
	TokenVersion       int        `json:"-"`
	Locale             string     `json:"locale,omitempty"`
	HasSavedPreference bool       `json:"has_saved_preference"`
	Theme              *string    `json:"theme,omitempty"`
	HasSavedTheme      bool       `json:"has_saved_theme"`
	DisabledAt         *time.Time `json:"disabled_at,omitempty"`
	Profile            string     `json:"profile,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	LastLoginAt        *time.Time `json:"last_login_at,omitempty"`
}

func (u *User) IsDisabled() bool {
	return u != nil && u.DisabledAt != nil
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

func (u *User) IsPlatformOps() bool {
	return u != nil && u.Role == RolePlatformOps
}

// UserIdentity represents a verified external Firebase identity linked to a canonical user.
type UserIdentity struct {
	ID                  uuid.UUID  `json:"id"`
	UserID              uuid.UUID  `json:"user_id"`
	FirebaseProjectID   string     `json:"firebase_project_id"`
	FirebaseUID         string     `json:"firebase_uid"`
	Provider            string     `json:"provider"`
	Status              string     `json:"status"`
	LinkMethod          string     `json:"link_method"`
	CreatedAt           time.Time  `json:"created_at"`
	LinkedAt            time.Time  `json:"linked_at"`
	LastAuthenticatedAt *time.Time `json:"last_authenticated_at,omitempty"`
}
