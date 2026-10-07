package billing

import (
	"context"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// DueRepository is the subset of due persistence used by billing.
type DueRepository interface {
	Create(ctx context.Context, d *domain.Due) error
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Due, error)
	Update(ctx context.Context, d *domain.Due) error
	HasOpenRentDue(ctx context.Context, tenantID uuid.UUID) (bool, error)
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Due, error)
}

// TenantRepository loads and updates tenants (credit balance, etc.).
type TenantRepository interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)
	GetByIDForUpdate(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)
	DeductCredit(ctx context.Context, tenantID uuid.UUID, amountPaise int64) (int64, error)
	Update(ctx context.Context, t *domain.Tenant) error
}
