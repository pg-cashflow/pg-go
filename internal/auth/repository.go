package auth

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// TenantRepository is the subset of postgres.TenantRepo used by auth.
type TenantRepository interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)
	GetByPhone(ctx context.Context, phone string) (*domain.Tenant, error)
}

// UserLookup is the subset of UserRepository used by JWT middleware
// for live token_version checks. Nil is allowed in tests that skip revocation.
type UserLookup interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error)
}

// SessionStore supports live JWT checks and POST /auth/revoke-sessions.
type SessionStore interface {
	UserLookup
	IncrementTokenVersion(ctx context.Context, id uuid.UUID) error
}

// UserRepository is the subset of postgres.UserRepo used by auth.
type UserRepository interface {
	Create(ctx context.Context, u *domain.User) error
	GetByPhone(ctx context.Context, phone string) (*domain.User, error)
	GetByEmail(ctx context.Context, email string) (*domain.User, error)
	GetByFirebaseUID(ctx context.Context, firebaseUID string) (*domain.User, error)
	GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error)
	LinkFirebaseUID(ctx context.Context, userID uuid.UUID, firebaseUID string) error
	TouchLogin(ctx context.Context, id uuid.UUID) error
	IncrementTokenVersion(ctx context.Context, id uuid.UUID) error
}

// OTPRepository is the subset of postgres.OTPRepo used by auth.
type OTPRepository interface {
	Create(ctx context.Context, req *postgres.OTPRequest) error
	LatestUnused(ctx context.Context, phone string) (*postgres.OTPRequest, error)
	IncrementAttempts(ctx context.Context, id uuid.UUID) error
	MarkUsed(ctx context.Context, id uuid.UUID) error
	CountRecent(ctx context.Context, phone string, since time.Time) (int, error)
}

// PropertyRepository looks up properties by owner phone or email for first-login user creation.
type PropertyRepository interface {
	GetByOwnerPhone(ctx context.Context, phone string) (*domain.Property, error)
	GetByOwnerEmail(ctx context.Context, email string) (*domain.Property, error)
	GetByInviteCode(ctx context.Context, code string) (*domain.Property, error)
}
