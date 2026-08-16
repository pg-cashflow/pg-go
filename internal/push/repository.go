package push

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Subscription is a Web Push endpoint for a tenant.
type Subscription struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Endpoint  string
	P256dh    string
	Auth      string
	CreatedAt time.Time
}

// Repository persists push subscriptions.
type Repository interface {
	Upsert(ctx context.Context, s *Subscription) error
	ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]Subscription, error)
	DeleteByEndpoint(ctx context.Context, endpoint string) error
	DeleteByTenant(ctx context.Context, tenantID uuid.UUID) error
}
