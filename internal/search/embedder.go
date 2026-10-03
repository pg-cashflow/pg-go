package search

import (
	"context"
	"errors"
)

// ErrEmbeddingsNotConfigured is returned by NoopEmbedder; callers treat it as "text-only".
var ErrEmbeddingsNotConfigured = errors.New("embeddings not configured")

// Embedder produces dense vectors for semantic search.
// Kept as a no-op interface so cmd/server wiring compiles; the search path never calls it.
type Embedder interface {
	Dimensions() int
	Embed(ctx context.Context, text string) ([]float32, error)
}

// NoopEmbedder disables vector search (the only supported embedder in production per ADR-012).
type NoopEmbedder struct{}

func (NoopEmbedder) Dimensions() int { return 384 }

func (NoopEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, ErrEmbeddingsNotConfigured
}
