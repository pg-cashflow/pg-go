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
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// Polls stale Cashfree payment_intents and settles successful captures.
// No-op when CASHFREE_* keys are unset (dormant).
func main() {
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	cfCfg := cashfree.Config{AppID: cfg.CashfreeAppID, SecretKey: cfg.CashfreeSecretKey, Env: cfg.CashfreeEnv}
	if !cfCfg.Enabled() {
		slog.Default().Info("cashfree poll skipped: not configured")
		os.Exit(0)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	intentRepo := postgres.NewPaymentIntentRepo(pool)
	dueRepo := postgres.NewDueRepo(pool)
	paymentRepo := postgres.NewPaymentRepo(pool)
	tenantRepo := postgres.NewTenantRepo(pool)
	eventRepo := postgres.NewEventRepo(pool)
	paySvc := payment.NewServiceWithPool(pool, dueRepo, paymentRepo, tenantRepo, eventRepo, payment.NewSQLSummaryRepository(pool))

	job := &jobs.CashfreePollJob{
		Intents: intentRepo,
		Dues:    dueRepo,
		Client:  cashfree.NewClient(cfCfg),
		Settle:  paySvc,
		Log:     slog.Default(),
	}
	if err := job.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
