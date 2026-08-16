package events

import (
	"context"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

// NoopPublisher discards events (tests / local without DB).
type NoopPublisher struct{}

func (NoopPublisher) Publish(context.Context, domain.Event) error { return nil }

var _ Publisher = NoopPublisher{}
