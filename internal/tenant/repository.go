package tenant

import (
	"context"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// Repository persists tenants.
type Repository interface {
	Create(ctx context.Context, t *domain.Tenant) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)
	Update(ctx context.Context, t *domain.Tenant) error
}

// PushRepository deletes push subscriptions on vacate.
type PushRepository interface {
	DeleteByTenant(ctx context.Context, tenantID uuid.UUID) error
}

// DepositDueCreator creates the onboarding deposit due.
type DepositDueCreator interface {
	CreateDepositDue(ctx context.Context, tenant *domain.Tenant, amountPaise int64) (*domain.Due, error)
}
