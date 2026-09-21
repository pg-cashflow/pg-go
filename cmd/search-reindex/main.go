package main

import (
	"context"
	"log"
	"os"

	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/search"
)

// Rebuilds search_documents embeddings for all properties.
// Usage: SEARCH_EMBEDDING=hash go run ./cmd/search-reindex/
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

	var embedder search.Embedder = search.HashEmbedder{}
	if os.Getenv("SEARCH_EMBEDDING") == "none" {
		embedder = search.NoopEmbedder{}
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
