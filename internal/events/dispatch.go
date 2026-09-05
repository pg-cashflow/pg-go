package events

import (
	"context"
	"log/slog"
	"sync"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

type EventHook func(e domain.Event)

// DispatchPublisher wraps a Publisher and dispatches events to async hooks.
type DispatchPublisher struct {
	inner Publisher
	mu    sync.RWMutex
	hooks []EventHook
}

func NewDispatchPublisher(inner Publisher) *DispatchPublisher {
	return &DispatchPublisher{
		inner: inner,
	}
}

func (d *DispatchPublisher) Subscribe(hook EventHook) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.hooks = append(d.hooks, hook)
}

func (d *DispatchPublisher) Publish(ctx context.Context, e domain.Event) error {
	// Synchronously persist event
	err := d.inner.Publish(ctx, e)
	if err != nil {
		return err
	}

	// Asynchronously notify subscribers
	d.mu.RLock()
	hooks := make([]EventHook, len(d.hooks))
	copy(hooks, d.hooks)
	d.mu.RUnlock()

	for _, h := range hooks {
		hook := h
		go func() {
			defer func() {
				if r := recover(); r != nil {
					slog.Default().Error("event hook panic recovered", "panic", r, "event_type", e.EventType)
				}
			}()
			hook(e)
		}()
	}

	return nil
}

var _ Publisher = (*DispatchPublisher)(nil)
