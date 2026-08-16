package main

import (
	"context"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/aadhaar"
	"github.com/pg-cashflow/pg-go/internal/api"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/billing"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/collector"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/events"
	joinsvc "github.com/pg-cashflow/pg-go/internal/join"
	"github.com/pg-cashflow/pg-go/internal/magiclink"
	"github.com/pg-cashflow/pg-go/internal/mailer"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/push"
	"github.com/pg-cashflow/pg-go/internal/sms"
	"github.com/pg-cashflow/pg-go/internal/tenant"
)

func main() {
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	pool, err := postgres.NewPool(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	if os.Getenv("AUTO_MIGRATE") == "1" || os.Getenv("AUTO_MIGRATE") == "true" {
		dir := os.Getenv("MIGRATIONS_DIR")
		if dir == "" {
			dir = "migrations"
			if _, err := os.Stat(dir); err != nil {
				dir = filepath.Join("..", "..", "migrations")
			}
		}
		if err := postgres.Migrate(ctx, pool, dir); err != nil {
			log.Fatal("migrate: ", err)
		}
	}

	logger := slog.Default()

	propertyRepo := postgres.NewPropertyRepo(pool)
	tenantRepo := postgres.NewTenantRepo(pool)
	dueRepo := postgres.NewDueRepo(pool)
	paymentRepo := postgres.NewPaymentRepo(pool)
	eventRepo := postgres.NewEventRepo(pool)
	userRepo := postgres.NewUserRepo(pool)
	otpRepo := postgres.NewOTPRepo(pool)
	tokenRepo := postgres.NewTokenRepo(pool)
	pushRepo := postgres.NewPushRepo(pool)
	importRepo := postgres.NewImportRepo(pool)
	joinRepo := postgres.NewJoinRepo(pool)
	reportRepo := postgres.NewPaymentReportRepo(pool)
	intentRepo := postgres.NewPaymentIntentRepo(pool)

	if cfg.AadhaarQRPublicKeyPEM != "" {
		if err := aadhaar.SetSecureQRPublicKeyPEM(cfg.AadhaarQRPublicKeyPEM); err != nil {
			log.Fatal("aadhaar public key: ", err)
		}
	}

	pub := events.NewPostgresPublisher(eventRepo)

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

	authSvc := auth.NewService(otpRepo, userRepo, tenantRepo, propertyRepo, gateway, cfg.OTPHMACSecret, cfg.JWTSecret)
	if cfg.FirebaseProjectID != "" {
		if cfg.FirebaseCredentials != "" {
			if _, err := os.Stat(cfg.FirebaseCredentials); err != nil {
				logger.Error("firebase credentials file missing", "path", cfg.FirebaseCredentials, "err", err)
			}
		}
		fbVerifier, err := auth.NewFirebaseVerifier(ctx, cfg.FirebaseProjectID, cfg.FirebaseCredentials)
		if err != nil {
			if cfg.AppEnv == "production" {
				log.Fatal("firebase: ", err)
			}
			logger.Error("firebase auth disabled", "err", err, "credentials", cfg.FirebaseCredentials)
		} else {
			authSvc.SetFirebaseVerifier(fbVerifier)
			logger.Info("firebase phone auth enabled", "project", cfg.FirebaseProjectID)
		}
	}
	billingSvc := billing.NewService(dueRepo, tenantRepo, pub)
	paySvc := payment.NewServiceWithPool(pool, dueRepo, paymentRepo, tenantRepo, eventRepo, payment.NewSQLSummaryRepository(pool))
	tenantSvc := tenant.NewServiceWithPool(pool, tenantRepo, dueRepo, eventRepo, pushRepo)
	magicSvc := magiclink.NewService(tokenRepo, dueRepo, propertyRepo, tenantRepo, cfg.MagicLinkHMACSecret)
	pushSvc := push.NewService(push.RepoAdapter{Inner: pushRepo}, push.Config{
		VAPIDPublicKey:  cfg.VAPIDPublicKey,
		VAPIDPrivateKey: cfg.VAPIDPrivateKey,
		Subject:         cfg.VAPIDSubject,
	}, logger)

	joinSvc := joinsvc.NewServiceWithPool(pool, propertyRepo, joinRepo, userRepo, tenantSvc, eventRepo)

	var cfOrders collector.CashfreeOrders
	cfCfg := cashfree.Config{AppID: cfg.CashfreeAppID, SecretKey: cfg.CashfreeSecretKey, Env: cfg.CashfreeEnv}
	if cfCfg.Enabled() {
		cfOrders = cashfree.NewClient(cfCfg)
		logger.Info("cashfree orders enabled", "env", cfg.CashfreeEnv)
	}
	collectorSvc := collector.New(intentRepo, cfOrders)

	router := api.NewRouter(api.Deps{
		JWTSecret:          cfg.JWTSecret,
		Auth:               authSvc,
		MagicLink:          magicSvc,
		Push:               pushSvc,
		Tenants:            tenantSvc,
		Billing:            billingSvc,
		Payments:           paySvc,
		PropertyStore:      propertyRepo,
		TenantStore:        tenantRepo,
		DueStore:           dueRepo,
		PaymentStore:       paymentRepo,
		EventStore:         eventRepo,
		ImportStore:        importRepo,
		Joins:              joinSvc,
		ReportStore:        reportRepo,
		IntentStore:        intentRepo,
		PaymentLookup:      paymentRepo,
		Collector:          collectorSvc,
		CashfreeSecret:     cfg.CashfreeWebhookSecret,
		CashfreeEnv:        cfg.CashfreeEnv,
		AuthTenantRepo:     tenantRepo,
		Events:             pub,
		MagicLinkBaseURL:   cfg.MagicLinkBaseURL,
		VAPIDPublicKey:     cfg.VAPIDPublicKey,
		CORSAllowedOrigins: cfg.CORSAllowedOrigins,
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		logger.Info("listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
