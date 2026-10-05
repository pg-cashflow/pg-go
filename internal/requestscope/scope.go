package requestscope

import (
	"context"

	"github.com/google/uuid"
)

type contextKey struct{}

var propertyKey = contextKey{}

// WithPropertyID returns a new context with the given propertyID attached.
func WithPropertyID(ctx context.Context, propertyID uuid.UUID) context.Context {
	return context.WithValue(ctx, propertyKey, propertyID)
}

// PropertyIDFromContext returns the propertyID from the context if present.
func PropertyIDFromContext(ctx context.Context) (uuid.UUID, bool) {
	val := ctx.Value(propertyKey)
	if val == nil {
		return uuid.Nil, false
	}
	id, ok := val.(uuid.UUID)
	if !ok || id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}
