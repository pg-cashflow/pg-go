package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/app"
	"github.com/pg-cashflow/pg-go/internal/billing"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/jobs"
	"github.com/pg-cashflow/pg-go/internal/magiclink"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/push"
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

	tenantRepo := postgres.NewTenantRepo(pool)
	dueRepo := postgres.NewDueRepo(pool)
	propertyRepo := postgres.NewPropertyRepo(pool)
	tokenRepo := postgres.NewTokenRepo(pool)
	eventRepo := postgres.NewEventRepo(pool)
	pushRepo := postgres.NewPushRepo(pool)
	pub := events.NewPostgresPublisher(eventRepo)

	billingSvc := billing.NewServiceWithPool(pool, dueRepo, tenantRepo, eventRepo)
	magicSvc := magiclink.NewService(tokenRepo, dueRepo, propertyRepo, tenantRepo, cfg.MagicLinkHMACSecret)

	channels := app.OpenChannels(cfg)
	mail := channels.Mail
	gateway := channels.Gateway
	pushSvc := push.NewService(push.RepoAdapter{Inner: pushRepo}, push.Config{
		VAPIDPublicKey:  cfg.VAPIDPublicKey,
		VAPIDPrivateKey: cfg.VAPIDPrivateKey,
		Subject:         cfg.VAPIDSubject,
	}, slog.Default())

	var recurringScheduler jobs.RecurringExpenseScheduler
	if cfg.FinanceEnabled {
		financeRepo := postgres.NewFinanceRepo(pool)
		finSvc := finance.NewService(financeRepo, pub)
		recurringScheduler = finance.NewRecurringExpenseScheduler(finSvc, financeRepo, propertyRepo)
	}

	job := &jobs.BillingCycle{
		Billing:           billingSvc,
		Tenants:           tenantRepo,
		Properties:        propertyRepo,
		MagicLink:         magicSvc,
		SMS:               gateway,
		Push:              pushSvc,
		RecurringExpenses: recurringScheduler,
		Mailer:            mail,
		Events:            pub,
		BaseURL:           cfg.MagicLinkBaseURL,
		Log:               slog.Default(),
		ErrOpenDueExists:  billing.ErrOpenDueExists,
	}

	release, acquired, err := jobs.AcquireJobLock(ctx, pool, "billing_cycle")
	if err != nil {
		log.Fatalf("failed to acquire job lock: %v", err)
	}
	if !acquired {
		slog.Info("another billing-cycle job is currently running, exiting gracefully")
		os.Exit(0)
	}
	defer release(context.Background())

	if err := job.Run(ctx); err != nil {
		log.Fatal(err)
	}
	os.Exit(0)
}
