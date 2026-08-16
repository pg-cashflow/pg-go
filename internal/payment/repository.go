package payment

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// DueRepository is the subset of due persistence used by payment matching/settlement.
type DueRepository interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Due, error)
	GetByDueCode(ctx context.Context, code string) (*domain.Due, error)
	Update(ctx context.Context, d *domain.Due) error
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Due, error)
	FindByAmountAndDateWindow(ctx context.Context, propertyID uuid.UUID, amount int, from, to time.Time) ([]domain.Due, error)
}

// PaymentRepository persists matched payments.
type PaymentRepository interface {
	Create(ctx context.Context, p *domain.Payment) error
	GetByUPITxnID(ctx context.Context, txnID string) (*domain.Payment, error)
}

// TenantRepository loads/updates tenants for credit balance.
type TenantRepository interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)
	Update(ctx context.Context, t *domain.Tenant) error
}

// SummaryRepository runs the reconciliation aggregate query.
type SummaryRepository interface {
	QueryReconciliation(ctx context.Context, propertyID uuid.UUID, from, to time.Time) (*ReconciliationSummary, error)
}
