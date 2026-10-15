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
	"github.com/pg-cashflow/pg-go/internal/jobs"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/waitlist"
)

func main() {
	flag.Parse()
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("waitlist-offer-expiry: config load: %v", err)
	}
	if err := cfg.ValidateForRealDeployment(); err != nil {
		log.Fatalf("waitlist-offer-expiry: validation: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseWorkerURL)
	if err != nil {
		log.Fatalf("waitlist-offer-expiry: postgres connection: %v", err)
	}
	defer pool.Close()
	if err := postgres.ValidateWorkerRole(ctx, pool, cfg.AppEnv); err != nil {
		log.Fatalf("waitlist-offer-expiry: worker role validation: %v", err)
	}

	const lockKey = 0x574149544C5354 // "WAITLST"
	var acquired bool
	if err := pool.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&acquired); err != nil {
		log.Fatalf("waitlist-offer-expiry: acquire advisory lock: %v", err)
	}
	if !acquired {
		slog.Info("waitlist-offer-expiry: another instance is already running; skipping")
		return
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey)
	}()

	waitlistRepo := postgres.NewWaitlistRepo(pool)
	slotRepo := postgres.NewSlotInviteRepo(pool)
	settings := postgres.NewOnboardingSettingsRepo(pool)
	svc := waitlist.NewService(pool, waitlistRepo, slotRepo, settings)
	job := jobs.NewWaitlistOfferExpiryJob(svc)

	n, err := job.Run(ctx)
	if err != nil {
		slog.Error("waitlist-offer-expiry job failed", "err", err)
		os.Exit(1)
	}
	slog.Info("waitlist-offer-expiry job finished", "expired_offers", n)
}
