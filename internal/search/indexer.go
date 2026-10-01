package search

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// Indexer rebuilds search_documents for a property.
type Indexer struct {
	Repo     IndexRepository
	Embedder Embedder
}

// RebuildProperty refreshes search_documents for a property. Embeddings are optional:
// with no embedder (or NoopEmbedder) documents are stored text-only so lexical search works.
// Any other embedder error is returned.
func (idx *Indexer) RebuildProperty(ctx context.Context, propertyID uuid.UUID) error {
	sources, err := idx.Repo.ListIndexSources(ctx, propertyID)
	if err != nil {
		return err
	}
	for _, s := range sources {
		var vec []float32
		if idx.Embedder != nil {
			v, err := idx.Embedder.Embed(ctx, s.Title+"\n"+s.Body)
			switch {
			case err == nil:
				vec = v
			case errors.Is(err, ErrEmbeddingsNotConfigured):
			default:
				return err
			}
		}
		if err := idx.Repo.UpsertDocument(ctx, s.PropertyID, s.TenantID, s.EntityType, s.EntityID, s.Title, s.Body, vec); err != nil {
			return err
		}
	}
	return nil
}
