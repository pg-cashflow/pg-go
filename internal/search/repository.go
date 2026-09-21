package search

import (
	"context"

	"github.com/google/uuid"
)

// Repository runs lexical and vector queries scoped by property/tenant.
type Repository interface {
	SearchLexical(ctx context.Context, p Params, types []EntityType, perType int) ([]Result, error)
	SearchVector(ctx context.Context, p Params, perType int, embedding []float32) ([]Result, error)
}

// IndexRepository upserts semantic documents.
type IndexRepository interface {
	UpsertDocument(ctx context.Context, propertyID uuid.UUID, tenantID *uuid.UUID, entityType string, entityID uuid.UUID, title, body string, embedding []float32) error
	DeleteDocument(ctx context.Context, propertyID uuid.UUID, entityType string, entityID uuid.UUID) error
	ListIndexSources(ctx context.Context, propertyID uuid.UUID) ([]IndexSource, error)
}

// IndexSource is one row to embed and store.
type IndexSource struct {
	PropertyID uuid.UUID
	TenantID   *uuid.UUID
	EntityType string
	EntityID   uuid.UUID
	Title      string
	Body       string
}
