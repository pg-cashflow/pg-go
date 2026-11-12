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
	"github.com/pg-cashflow/pg-go/internal/slotinvite"
)

func main() {
	flag.Parse()
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("slot-invite-expiry: config load: %v", err)
	}
	if err := cfg.ValidateForRealDeployment(); err != nil {
		log.Fatalf("slot-invite-expiry: validation: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseWorkerURL)
	if err != nil {
		log.Fatalf("slot-invite-expiry: postgres connection: %v", err)
	}
	defer pool.Close()
	if err := postgres.ValidateWorkerRole(ctx, pool, cfg.AppEnv); err != nil {
		log.Fatalf("slot-invite-expiry: worker role validation: %v", err)
	}

	release, acquired, err := jobs.AcquireJobLock(ctx, pool, "slot_invite_expiry")
	if err != nil {
		log.Fatalf("slot-invite-expiry: acquire job lock: %v", err)
	}
	if !acquired {
		slog.Info("slot-invite-expiry: another instance is already running; skipping")
		return
	}
	defer func() {
		_ = release(context.Background())
	}()

	repo := postgres.NewSlotInviteRepo(pool)
	settings := postgres.NewOnboardingSettingsRepo(pool)
	svc := slotinvite.NewService(pool, repo, settings)
	job := jobs.NewSlotInviteExpiryJob(svc)

	n, err := job.Run(ctx)
	if err != nil {
		slog.Error("slot-invite-expiry job failed", "err", err)
		os.Exit(1)
	}
	slog.Info("slot-invite-expiry job finished", "expired", n)
}
