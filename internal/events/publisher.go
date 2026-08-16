package events

import (
	"context"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

// Publisher persists domain events for the audit log.
type Publisher interface {
	Publish(ctx context.Context, e domain.Event) error
}
