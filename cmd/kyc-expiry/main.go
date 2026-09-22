package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/jobs"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

func main() {
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("kyc-expiry: config load: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("kyc-expiry: postgres connection: %v", err)
	}
	defer pool.Close()

	// Overlap guard: acquire session-level advisory lock to prevent concurrent executions
	const kycExpiryLockKey = 0x4B5943455850 // "KYCEXP" in hex
	var acquired bool
	err = pool.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, kycExpiryLockKey).Scan(&acquired)
	if err != nil {
		log.Fatalf("kyc-expiry: acquire advisory lock: %v", err)
	}
	if !acquired {
		slog.Info("kyc-expiry: another instance is already running; skipping sweep")
		return
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, kycExpiryLockKey)
	}()

	repo := postgres.NewKYCRepo(pool)
	job := jobs.NewKYCExpiryJob(repo)

	now := time.Now().UTC()
	// TTL for pending sessions is set to 24h to safely exceed Cashfree's max webhook retry
	// backoff window (~24 hours). This prevents the reaper from prematurely marking a verification
	// 'expired' while a delayed/retrying completion webhook is still in flight.
	pendingTTL := 24 * time.Hour

	if err := job.Run(ctx, now, pendingTTL); err != nil {
		slog.Error("kyc-expiry job failed", "err", err)
		os.Exit(1)
	}

	slog.Info("kyc-expiry job finished successfully")
}
