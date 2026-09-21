package main

import (
	"context"
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
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

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

	if err := job.Run(ctx); err != nil {
		log.Fatal(err)
	}
	os.Exit(0)
}
