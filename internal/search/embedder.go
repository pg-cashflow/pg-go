package search

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"math"
)

const EmbeddingDims = 384

// Embedder produces dense vectors for semantic search.
type Embedder interface {
	Dimensions() int
	Embed(ctx context.Context, text string) ([]float32, error)
}

// ErrEmbeddingsNotConfigured is returned by NoopEmbedder; callers treat it as "text-only".
var ErrEmbeddingsNotConfigured = errors.New("embeddings not configured")

// NoopEmbedder disables vector search.
type NoopEmbedder struct{}

func (NoopEmbedder) Dimensions() int { return EmbeddingDims }

func (NoopEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, ErrEmbeddingsNotConfigured
}

// HashEmbedder is a deterministic local embedder for dev/tests (not true semantics).
type HashEmbedder struct{}

func (HashEmbedder) Dimensions() int { return EmbeddingDims }

func (HashEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	sum := sha256.Sum256([]byte(text))
	vec := make([]float32, EmbeddingDims)
	for i := 0; i < EmbeddingDims; i++ {
		idx := (i * 4) % len(sum)
		u := binary.BigEndian.Uint32(sum[idx : idx+4])
		vec[i] = float32(int32(u%1000)) / 1000.0
	}
	norm := 0.0
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm > 0 {
		inv := float32(1.0 / math.Sqrt(norm))
		for i := range vec {
			vec[i] *= inv
		}
	}
	return vec, nil
}
