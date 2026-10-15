package waitlist

import (
	"context"
	"log/slog"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type TenantRoomLookup interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error)
}

type EventConsumer struct {
	svc     *Service
	tenants TenantRoomLookup
	logger  *slog.Logger
}

func NewEventConsumer(svc *Service, tenants TenantRoomLookup, logger *slog.Logger) *EventConsumer {
	return &EventConsumer{svc: svc, tenants: tenants, logger: logger}
}

func (c *EventConsumer) ProcessEvent(ctx context.Context, e domain.Event) error {
	if c.svc == nil || c.tenants == nil || e.EventType != domain.EvtTenantVacated || e.TenantID == nil {
		return nil
	}
	t, err := c.tenants.GetByID(ctx, *e.TenantID)
	if err != nil || t.RoomID == nil {
		return nil
	}
	if err := c.svc.TryOfferAfterVacate(ctx, t.PropertyID, *t.RoomID); err != nil {
		c.logger.Warn("waitlist vacate offer failed", "tenant_id", e.TenantID, "err", err)
	}
	return nil
}

func (c *EventConsumer) ProcessEventAsync(e domain.Event) {
	go func() {
		_ = c.ProcessEvent(context.Background(), e)
	}()
}
