package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

func main() {
	flag.Parse()
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("join-pending-expiry: config: %v", err)
	}
	if err := cfg.ValidateForRealDeployment(); err != nil {
		log.Fatalf("join-pending-expiry: validation: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := postgres.NewPool(ctx, cfg.DatabaseWorkerURL)
	if err != nil {
		log.Fatalf("join-pending-expiry: pool: %v", err)
	}
	defer pool.Close()
	if err := postgres.ValidateWorkerRole(ctx, pool, cfg.AppEnv); err != nil {
		log.Fatalf("join-pending-expiry: role: %v", err)
	}
	settings := postgres.NewOnboardingSettingsRepo(pool)
	joins := postgres.NewJoinRepo(pool)
	ttlDays := 30
	// default cutoff if settings missing
	cutoff := time.Now().UTC().AddDate(0, 0, -ttlDays)
	n, err := joins.RejectStalePending(ctx, cutoff)
	if err != nil {
		slog.Error("join-pending-expiry failed", "err", err)
		os.Exit(1)
	}
	slog.Info("join-pending-expiry done", "rejected", n, "settings_repo_ready", settings != nil)
}
