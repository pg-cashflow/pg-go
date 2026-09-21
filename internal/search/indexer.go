package search

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Indexer rebuilds search_documents for a property.
type Indexer struct {
	Repo     IndexRepository
	Embedder Embedder
}

// RebuildProperty re-embeds all indexable entities for a property.
func (idx *Indexer) RebuildProperty(ctx context.Context, propertyID uuid.UUID) error {
	if idx.Embedder == nil {
		return fmt.Errorf("embedder not configured")
	}
	sources, err := idx.Repo.ListIndexSources(ctx, propertyID)
	if err != nil {
		return err
	}
	for _, s := range sources {
		text := s.Title + "\n" + s.Body
		vec, err := idx.Embedder.Embed(ctx, text)
		if err != nil {
			return err
		}
		if err := idx.Repo.UpsertDocument(ctx, s.PropertyID, s.TenantID, s.EntityType, s.EntityID, s.Title, s.Body, vec); err != nil {
			return err
		}
	}
	return nil
}
