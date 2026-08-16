package magiclink

import (
	"context"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// TokenRepository persists payment magic-link tokens.
type TokenRepository interface {
	Create(ctx context.Context, t *domain.PaymentToken) error
	InvalidateUnusedForDue(ctx context.Context, dueID uuid.UUID) error
	GetByHash(ctx context.Context, hash string) (*domain.PaymentToken, error)
	MarkUsed(ctx context.Context, id uuid.UUID) error
}

// DueRepository loads dues for magic-link resolution.
type DueRepository interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Due, error)
}

// PropertyRepository loads property owner display fields.
type PropertyRepository interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Property, error)
}

// TenantRepository loads tenant display fields. Status is intentionally unused.
type TenantRepository interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)
}
