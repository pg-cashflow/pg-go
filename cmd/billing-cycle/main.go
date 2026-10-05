package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/billing"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/jobs"
	"github.com/pg-cashflow/pg-go/internal/magiclink"
	"github.com/pg-cashflow/pg-go/internal/mailer"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/push"
	"github.com/pg-cashflow/pg-go/internal/sms"
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

	tenantRepo := postgres.NewTenantRepo(pool)
	dueRepo := postgres.NewDueRepo(pool)
	propertyRepo := postgres.NewPropertyRepo(pool)
	tokenRepo := postgres.NewTokenRepo(pool)
	eventRepo := postgres.NewEventRepo(pool)
	pushRepo := postgres.NewPushRepo(pool)
	pub := events.NewPostgresPublisher(eventRepo)

	billingSvc := billing.NewService(dueRepo, tenantRepo, pub)
	magicSvc := magiclink.NewService(tokenRepo, dueRepo, propertyRepo, tenantRepo, cfg.MagicLinkHMACSecret)

	var mail mailer.Mailer = mailer.NoopMailer{}
	if cfg.SMTPHost != "" {
		mail = &mailer.SMTPMailer{
			Host: cfg.SMTPHost, Port: cfg.SMTPPort,
			Username: cfg.SMTPUsername, Password: cfg.SMTPPassword, From: cfg.SMTPFrom,
		}
	}

	var gateway sms.SMSGateway = sms.NoopGateway{}
	if cfg.SMSPrimaryURL != "" {
		gateway = &sms.AndroidGateway{
			PrimaryURL:     cfg.SMSPrimaryURL,
			PrimaryAPIKey:  cfg.SMSPrimaryAPIKey,
			FallbackURL:    cfg.SMSFallbackURL,
			FallbackAPIKey: cfg.SMSFallbackAPIKey,
			Alert:          sms.MailAlert(mail, cfg.SMTPFrom),
		}
	}
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
		Billing:          billingSvc,
		Tenants:          tenantRepo,
		Properties:       propertyRepo,
		MagicLink:        magicSvc,
		SMS:              gateway,
		Push:             pushSvc,
		RecurringExpenses: recurringScheduler,
		Mailer:           mail,
		Events:           pub,
		BaseURL:          cfg.MagicLinkBaseURL,
		Log:              slog.Default(),
		ErrOpenDueExists: billing.ErrOpenDueExists,
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
