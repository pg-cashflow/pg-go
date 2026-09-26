package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

func main() {
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dbURL := os.Getenv("DATABASE_URL_UNPOOLED")
	if dbURL == "" {
		dbURL = os.Getenv("DIRECT_URL")
	}
	if dbURL == "" {
		dbURL = cfg.DatabaseURL
	}

	pool, err := postgres.NewPool(ctx, dbURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	dir := os.Getenv("MIGRATIONS_DIR")
	if dir == "" {
		dir = "migrations"
		if _, err := os.Stat(dir); err != nil {
			dir = filepath.Join("..", "..", "migrations")
		}
	}
	if err := postgres.Migrate(ctx, pool, dir); err != nil {
		log.Fatal(err)
	}
	fmt.Println("migrations applied")
}
