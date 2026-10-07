package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/jobs"
	"github.com/pg-cashflow/pg-go/internal/mailer"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/roi"
)

func main() {
	cadence := flag.String("cadence", "monthly", "monthly|yearly")
	flag.Parse()

	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	pool, err := postgres.NewPool(ctx, cfg.DatabaseMaintURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	propertyRepo := postgres.NewPropertyRepo(pool)
	tenantRepo := postgres.NewTenantRepo(pool)
	dueRepo := postgres.NewDueRepo(pool)
	paymentRepo := postgres.NewPaymentRepo(pool)
	eventRepo := postgres.NewEventRepo(pool)
	pub := events.NewPostgresPublisher(eventRepo)

	paySvc := payment.NewService(dueRepo, paymentRepo, tenantRepo, payment.NewSQLSummaryRepository(pool), pub)

	var mail mailer.Mailer = mailer.NoopMailer{}
	if cfg.SMTPHost != "" {
		mail = &mailer.SMTPMailer{
			Host:     cfg.SMTPHost,
			Port:     cfg.SMTPPort,
			Username: cfg.SMTPUsername,
			Password: cfg.SMTPPassword,
			From:     cfg.SMTPFrom,
		}
	}

	tmplDir := filepath.Join("internal", "mailer", "templates")
	if _, err := os.Stat(tmplDir); err != nil {
		tmplDir = filepath.Join("..", "..", "internal", "mailer", "templates")
	}

	var finSvc *finance.Service
	var roiSvc *roi.Service
	if cfg.FinanceEnabled {
		financeRepo := postgres.NewFinanceRepo(pool)
		finSvc = finance.NewService(financeRepo, pub)
		roiSvc = roi.NewService(finSvc)
	}

	var notifier finance.DeadLetterNotifier
	if cfg.AdminEmail != "" && mail != nil {
		notifier = finance.NewEmailDeadLetterNotifier(mail, cfg.AdminEmail)
	}

	job := &jobs.FinancialSummaryJob{
		Properties:  propertyRepo,
		Tenants:     tenantRepo,
		Summaries:   paySvc,
		Finance:     finSvc,
		ROI:         roiSvc,
		Mailer:      mail,
		Notifier:    notifier,
		Events:      pub,
		TemplateDir: tmplDir,
		Log:         slog.Default(),
	}
	if err := job.Run(ctx, *cadence); err != nil {
		log.Fatal(err)
	}
	os.Exit(0)
}
