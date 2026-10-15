package referral

import (
	"context"
	"log/slog"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

type EventConsumer struct {
	svc    *Service
	logger *slog.Logger
}

func NewEventConsumer(svc *Service, logger *slog.Logger) *EventConsumer {
	return &EventConsumer{svc: svc, logger: logger}
}

func (c *EventConsumer) ProcessEvent(ctx context.Context, e domain.Event) error {
	if c.svc == nil {
		return nil
	}
	switch e.EventType {
	case domain.EvtDuePaidOnTime, domain.EvtDuePaidLate:
		if e.DueID == nil {
			return nil
		}
		if err := c.svc.SettleFirstRentPayment(ctx, *e.DueID); err != nil {
			c.logger.Warn("referral settle failed", "due_id", e.DueID, "err", err)
		}
	}
	return nil
}

func (c *EventConsumer) ProcessEventAsync(e domain.Event) {
	go func() {
		_ = c.ProcessEvent(context.Background(), e)
	}()
}
