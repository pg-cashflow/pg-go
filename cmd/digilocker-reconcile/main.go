package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/jobs"
	"github.com/pg-cashflow/pg-go/internal/kyc"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

func main() {
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("digilocker-reconcile: config load: %v", err)
	}

	cfCfg := cashfree.Config{
		AppID:     cfg.CashfreeAppID,
		SecretKey: cfg.CashfreeSecretKey,
		Env:       cfg.CashfreeEnv,
	}
	if !cfCfg.Enabled() {
		slog.Info("digilocker-reconcile skipped: cashfree is not configured")
		os.Exit(0)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("digilocker-reconcile: postgres connection: %v", err)
	}
	defer pool.Close()

	// 64-bit advisory lock key allocated to DigiLocker reconciliation cron: ASCII 'DLKRECON' (0x444C4B5245434F4E).
	// Distinct from other application advisory locks in pg-go:
	//   - Schema migrations: 0x50474D4947524154 ('PGMIGRAT')
	//   - KYC expiry reaper: 0x4B5943455850     ('KYCEXP')
	const digiLockerReconcileLockKey int64 = 0x444C4B5245434F4E // 'DLKRECON' in hex
	var acquired bool
	err = pool.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, digiLockerReconcileLockKey).Scan(&acquired)
	if err != nil {
		log.Fatalf("digilocker-reconcile: acquire advisory lock: %v", err)
	}
	if !acquired {
		slog.Info("digilocker-reconcile: another instance is already running; skipping sweep")
		return
	}
	defer func() {
		_, _ = pool.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, digiLockerReconcileLockKey)
	}()

	repo := postgres.NewKYCRepo(pool)
	cfClient := cashfree.NewClient(cfCfg)
	identitySecret := cfg.KYCIdentitySecret
	if identitySecret == "" {
		identitySecret = cfg.JWTSecret
	}
	kycSvc := kyc.NewService(repo, cfClient, kyc.Config{
		IdentitySecret:           identitySecret,
		HashKeyVersion:           1,
		VerificationValidityDays: 365,
		DigiLockerRedirectURL:    cfg.KYCDigiLockerRedirectURL,
	})

	job := &jobs.DigiLockerReconcileJob{
		Repo:      repo,
		Cashfree:  cfClient,
		Completer: kycSvc,
		Log:       slog.Default(),
	}

	if err := job.Run(ctx); err != nil {
		slog.Error("digilocker-reconcile job failed", "err", err)
		os.Exit(1)
	}

	slog.Info("digilocker-reconcile job finished successfully")
}
