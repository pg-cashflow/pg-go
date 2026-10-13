package main

import (
	"context"
	"flag"
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
	urlFlag := flag.String("url", "", "database URL to run migrations against (overrides environment)")
	flag.Parse()
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Printf("warning: error loading .env: %v", err)
	}

	dbURL := *urlFlag
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL_UNPOOLED")
	}
	if dbURL == "" {
		dbURL = os.Getenv("DIRECT_URL")
	}
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_MAINT_URL")
	}
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_ADMIN_URL")
	}
	if dbURL == "" {
		dbURL = os.Getenv("DATABASE_URL")
	}
	if dbURL == "" {
		cfg, err := config.Load()
		if err != nil {
			log.Fatal(err)
		}
		dbURL = cfg.DatabaseMaintURL
	}
	if dbURL == "" {
		log.Fatal("database URL is required")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

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
