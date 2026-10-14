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
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/jobs"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/roi"
)

func main() {
	flag.Parse()
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if err := cfg.ValidateForRealDeployment(); err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseWorkerURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err := postgres.ValidateWorkerRole(ctx, pool, cfg.AppEnv); err != nil {
		log.Fatal(err)
	}

	propertyRepo := postgres.NewPropertyRepo(pool)
	tenantRepo := postgres.NewTenantRepo(pool)
	dueRepo := postgres.NewDueRepo(pool)
	paymentRepo := postgres.NewPaymentRepo(pool)
	eventRepo := postgres.NewEventRepo(pool)
	gamificationRepo := postgres.NewGamificationRepo(pool)
	pub := events.NewPostgresPublisher(eventRepo)

	paySvc := payment.NewService(dueRepo, paymentRepo, tenantRepo, payment.NewSQLSummaryRepository(pool), pub)

	financeRepo := postgres.NewFinanceRepo(pool)
	financeSvc := finance.NewService(financeRepo, pub)
	roiSvc := roi.NewService(financeSvc)

	job := &jobs.KPISnapshotJob{
		Properties: propertyRepo,
		Rooms:      gamificationRepo,
		Tenants:    tenantRepo,
		Summaries:  paySvc,
		ROI:        roiSvc,
		Log:        slog.Default(),
	}

	release, acquired, err := jobs.AcquireJobLock(ctx, pool, "kpi_snapshot")
	if err != nil {
		log.Fatalf("failed to acquire job lock: %v", err)
	}
	if !acquired {
		slog.Default().Info("another kpi snapshot job is currently running, exiting gracefully")
		os.Exit(0)
	}
	defer release(context.Background())

	if err := job.Run(ctx); err != nil {
		log.Fatal(err)
	}
	os.Exit(0)
}
