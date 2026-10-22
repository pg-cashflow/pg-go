package main

import (
	"context"
	"encoding/hex"
	"flag"
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
	flag.Parse()
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("kyc-expiry: config load: %v", err)
	}
	if err := cfg.ValidateForRealDeployment(); err != nil {
		log.Fatalf("kyc-expiry: validation: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseWorkerURL)
	if err != nil {
		log.Fatalf("kyc-expiry: postgres connection: %v", err)
	}
	defer pool.Close()
	if err := postgres.ValidateWorkerRole(ctx, pool, cfg.AppEnv); err != nil {
		log.Fatalf("kyc-expiry: worker role validation: %v", err)
	}

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

	var kycDocKey []byte
	if hex64 := cfg.KYCDocCryptoKey; hex64 != "" {
		if decoded, err := hex.DecodeString(hex64); err != nil || len(decoded) != 32 {
			slog.Warn("kyc-expiry: KYC_DOC_CRYPTO_KEY invalid; doc encryption disabled", "err", err)
		} else {
			kycDocKey = decoded
		}
	}
	repo := postgres.NewKYCRepo(pool, kycDocKey)
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

	refreshRepo := postgres.NewRefreshTokenRepo(pool)
	if n, err := refreshRepo.PurgeExpiredTokens(ctx, 24*time.Hour); err != nil {
		slog.Error("kyc-expiry: refresh token purge failed", "err", err)
	} else if n > 0 {
		slog.Info("kyc-expiry: purged expired refresh tokens", "deleted", n)
	}

	if n, err := jobs.TrimKYCFailureLog(ctx, pool); err != nil {
		slog.Error("kyc-expiry: failure log trim failed", "err", err)
	} else if n > 0 {
		slog.Info("kyc-expiry: trimmed kyc failure logs", "deleted", n)
	}

	if n, err := jobs.TrimRateLimitBuckets(ctx, pool); err != nil {
		slog.Error("kyc-expiry: rate limit bucket trim failed", "err", err)
	} else if n > 0 {
		slog.Info("kyc-expiry: trimmed rate limit buckets", "deleted", n)
	}

	slog.Info("kyc-expiry job finished successfully")
}
