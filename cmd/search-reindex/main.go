package main

import (
	"context"
	"log"
	"os"

	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/search"
)

// Rebuilds search_documents for all properties (text-only unless SEARCH_EMBEDDING=hash).
// Usage: go run ./cmd/search-reindex/
func main() {
	_ = godotenv.Load()
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL required")
	}
	ctx := context.Background()
	pool, err := postgres.NewPool(ctx, dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	// Text-only by default; SEARCH_EMBEDDING=hash adds dev-only vectors (needs pgvector).
	var embedder search.Embedder = search.NoopEmbedder{}
	if os.Getenv("SEARCH_EMBEDDING") == "hash" {
		embedder = search.HashEmbedder{}
	}

	repo := postgres.NewSearchRepo(pool)
	propRepo := postgres.NewPropertyRepo(pool)
	props, err := propRepo.List(ctx)
	if err != nil {
		log.Fatal(err)
	}
	idx := search.Indexer{Repo: repo, Embedder: embedder}
	for _, p := range props {
		if err := idx.RebuildProperty(ctx, p.ID); err != nil {
			log.Fatal("property ", p.ID, ": ", err)
		}
		log.Println("reindexed", p.Name)
	}
}
