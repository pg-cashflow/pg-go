package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/pg-cashflow/pg-go/internal/aadhaar"
	"github.com/pg-cashflow/pg-go/internal/api"
	"github.com/pg-cashflow/pg-go/internal/attendance"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/billing"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/collector"
	"github.com/pg-cashflow/pg-go/internal/config"
	"github.com/pg-cashflow/pg-go/internal/crypto"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/gamification"
	"github.com/pg-cashflow/pg-go/internal/intelligence"
	joinsvc "github.com/pg-cashflow/pg-go/internal/join"
	kycsvc "github.com/pg-cashflow/pg-go/internal/kyc"
	"github.com/pg-cashflow/pg-go/internal/magiclink"
	"github.com/pg-cashflow/pg-go/internal/mailer"
	notificationsvc "github.com/pg-cashflow/pg-go/internal/notification"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/push"
	roisvc "github.com/pg-cashflow/pg-go/internal/roi"
	"github.com/pg-cashflow/pg-go/internal/search"
	"github.com/pg-cashflow/pg-go/internal/sms"
	"github.com/pg-cashflow/pg-go/internal/tenant"
)

func main() {
	_ = godotenv.Load()
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	// ADR-2 Batch 1: credential-presence validation (not APP_ENV-keyed).
	if err := cfg.ValidateForRealDeployment(); err != nil {
		log.Fatal(err)
	}
	for _, w := range cfg.ValidateWarnings() {
		slog.Warn(w)
	}
	// ADR-2 M4: suppress header logging in production.
	if cfg.AppEnv == "production" {
		gin.SetMode(gin.ReleaseMode)
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
	preferencesRepo := postgres.NewPreferencesRepo(pool)

	if cfg.AadhaarQRPublicKeyPEM != "" {
		if err := aadhaar.SetSecureQRPublicKeyPEM(cfg.AadhaarQRPublicKeyPEM); err != nil {
			log.Fatal("aadhaar public key: ", err)
		}
	}

	gamificationRepo := postgres.NewGamificationRepo(pool)

	innerPub := events.NewPostgresPublisher(eventRepo)
	dispatchPub := events.NewDispatchPublisher(innerPub)
	pub := dispatchPub

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

	refreshRepo := postgres.NewRefreshTokenRepo(pool)
	authSvc := auth.NewService(otpRepo, userRepo, tenantRepo, propertyRepo, gateway, cfg.OTPHMACSecret, cfg.JWTSecret)
	authSvc.SetRefreshTokenRepo(refreshRepo)
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
	depositSettlementRepo := postgres.NewDepositSettlementRepo(pool)
	paySvc.SetDepositSettler(depositSettlementRepo)
	tenantSvc := tenant.NewServiceWithPool(pool, tenantRepo, dueRepo, eventRepo, pushRepo)
	magicSvc := magiclink.NewService(tokenRepo, dueRepo, propertyRepo, tenantRepo, cfg.MagicLinkHMACSecret)
	pushSvc := push.NewService(push.RepoAdapter{Inner: pushRepo}, push.Config{
		VAPIDPublicKey:  cfg.VAPIDPublicKey,
		VAPIDPrivateKey: cfg.VAPIDPrivateKey,
		Subject:         cfg.VAPIDSubject,
	}, logger)

	joinSvc := joinsvc.NewServiceWithPool(pool, propertyRepo, joinRepo, userRepo, tenantSvc, eventRepo)

	var cfOrders collector.CashfreeOrders
	var cfClient *cashfree.Client
	cfCfg := cashfree.Config{AppID: cfg.CashfreeAppID, SecretKey: cfg.CashfreeSecretKey, Env: cfg.CashfreeEnv}
	if cfCfg.Enabled() {
		cfClient = cashfree.NewClient(cfCfg)
		cfOrders = cfClient
		logger.Info("cashfree orders enabled", "env", cfg.CashfreeEnv)
	}
	collectorSvc := collector.New(intentRepo, cfOrders)

	var kycSvc api.KYCService
	if cfg.KYCIdentitySecret != "" {
		kycRepo := postgres.NewKYCRepo(pool)
		var cfKYCAdapter kycsvc.CashfreeKYCClient
		if cfClient != nil {
			cfKYCAdapter = kycsvc.NewCashfreeAdapter(cfClient)
			logger.Info("kyc service enabled", "digilocker", true)
		} else {
			logger.Warn("kyc service enabled in QR-only mode (Cashfree credentials not configured; DigiLocker unavailable)")
		}
		kycSvc = kycsvc.NewService(kycRepo, cfKYCAdapter, kycsvc.Config{
			IdentitySecret:           cfg.KYCIdentitySecret,
			HashKeyVersion:           1,
			VerificationValidityDays: 365,
			DigiLockerRedirectURL:    cfg.KYCDigiLockerRedirectURL,
		})
	} else {
		logger.Info("kyc service dormant (KYC_IDENTITY_SECRET not set)")
	}

	gamificationSvc := gamification.NewService(gamificationRepo, tenantRepo, dueRepo, dispatchPub, gamification.NewBlobStore())
	gamificationConsumer := gamification.NewEventConsumer(gamificationSvc)
	dispatchPub.Subscribe(gamificationConsumer.ProcessEventAsync)

	financeRepo := postgres.NewFinanceRepo(pool)
	financeSvc := finance.NewService(financeRepo, pub)
	roiSvc := roisvc.NewService(financeSvc)
	intelSvc := intelligence.NewService(financeRepo)
	if cfg.FinanceEnabled {
		paySvc.SetSettlementHook(func(ctx context.Context, p *domain.Payment, due *domain.Due) {
			if err := financeSvc.MirrorPayment(ctx, p, due); err != nil {
				logger.Error("LEDGER GAP: settlement hook MirrorPayment failed", "payment_id", p.ID, "due_id", due.ID, "err", err)
			}
		})
		billingSvc.SetProrateHook(func(ctx context.Context, due *domain.Due, original, prorated int64) {
			_ = financeSvc.MirrorProration(ctx, due, original, prorated)
		})
		gamificationSvc.SetFinanceHooks(
			func(ctx context.Context, tenant *domain.Tenant, entry *domain.PointsLedgerEntry, pointValuePaise int64) {
				id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("pts:%d", entry.ID)))
				amt := int64(entry.Delta) * pointValuePaise
				_ = financeSvc.MirrorPointsIssued(ctx, tenant.PropertyID, tenant.ID, id, entry.Delta, amt)
			},
			func(ctx context.Context, tenant *domain.Tenant, red *domain.Redemption, amountPaise int64) {
				_ = financeSvc.MirrorRewardRedeem(ctx, tenant.PropertyID, tenant.ID, red.ID, red.PointsSpent, amountPaise)
			},
		)
	}

	// Notification subsystem.
	outboxRepo := postgres.NewOutboxRepo(pool)
	notifRepo := postgres.NewNotificationRepo(pool)
	notifSvc := notificationsvc.NewService(notifRepo)
	resolver := notificationsvc.NewResolver(userRepo)
	dispatcher := notificationsvc.NewDispatcher(
		pool,
		&notificationsvc.CompositeRepo{OutboxRepo: outboxRepo, NotifRepo: notifRepo},
		resolver,
		notificationsvc.DefaultConfig(),
		logger,
	)

	searchPool, err := postgres.NewSearchPool(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Warn("failed to initialize dedicated search pool, falling back to main pool", "err", err)
		searchPool = pool
	} else {
		defer searchPool.Close()
	}
	searchRepo := postgres.NewSearchRepo(searchPool)
	// ADR-012: vector/hybrid search permanently disabled.
	searchSvc := &search.Service{Repo: searchRepo, Cache: search.NewCache(5*time.Second, 2000)}

	payoutRepo := postgres.NewPayoutRepo(pool, financeSvc)
	attendanceRepo := postgres.NewAttendanceRepo(pool)
	attendanceSvc := attendance.NewService(pool, attendanceRepo, payoutRepo)
	bankTxnRepo := postgres.NewBankTransactionRepo(pool)
	bankAccountRepo := postgres.NewBankAccountRepo(pool)
	settlementRepo := postgres.NewSettlementRepo(pool)
	settlementReconciler := finance.NewSettlementReconciler(settlementRepo, intentRepo, dueRepo, paymentRepo, financeSvc)
	settlementBalancerRepo := postgres.NewSettlementBalancerRepo(pool)
	settlementBalancer := finance.NewSettlementBalancer(settlementBalancerRepo)
	ledgerOutboxRepo := postgres.NewLedgerOutboxRepo(pool)

	payoutSecret := cfg.PayoutExportChecksumSecret
	if payoutSecret == "" {
		payoutSecret = cfg.JWTSecret
	}
	payoutEncryptionKey := crypto.DeriveKey(payoutSecret)

	var payoutDispatcher *finance.PayoutDispatcher
	if cfg.CashfreePayoutAutoDispatchEnabled && cfg.CashfreePayoutClientID != "" && cfg.CashfreePayoutClientSecret != "" {
		payoutClient := cashfree.NewPayoutClient(cashfree.PayoutConfig{
			ClientID:     cfg.CashfreePayoutClientID,
			ClientSecret: cfg.CashfreePayoutClientSecret,
			APIVersion:   cfg.CashfreePayoutAPIVersion,
			Env:          cfg.CashfreePayoutEnv,
		})
		payoutDispatcher = finance.NewPayoutDispatcher(payoutRepo, payoutClient, cfg.CashfreePayoutFundsourceID, payoutEncryptionKey)
		logger.Info("cashfree automated payouts dispatcher enabled", "env", cfg.CashfreePayoutEnv)
	} else if cfg.CashfreePayoutAutoDispatchEnabled {
		logger.Warn("CF_PAYOUT_AUTO_DISPATCH_ENABLED is true but Cashfree Payout credentials are not configured")
	}

	router := api.NewRouter(api.Deps{
		JWTSecret:                   cfg.JWTSecret,
		Auth:                        authSvc,
		MagicLink:                   magicSvc,
		Push:                        pushSvc,
		Tenants:                     tenantSvc,
		Billing:                     billingSvc,
		Payments:                    paySvc,
		Gamification:                gamificationSvc,
		GamificationStore:           gamificationRepo,
		UserStore:                   userRepo,
		PreferencesStore:            preferencesRepo,
		PropertyStore:               propertyRepo,
		TenantStore:                 tenantRepo,
		DueStore:                    dueRepo,
		PaymentStore:                paymentRepo,
		EventStore:                  eventRepo,
		ImportStore:                 importRepo,
		Joins:                       joinSvc,
		ReportStore:                 reportRepo,
		IntentStore:                 intentRepo,
		PaymentLookup:               paymentRepo,
		GatewayPaymentRepo:          paymentRepo,
		WebhookToleranceSec:         cfg.WebhookTimestampToleranceSec,
		WebhookAPIVersion:           cfg.WebhookAPIVersion,
		Pool:                        pool,
		Collector:                   collectorSvc,
		KYCSvc:                      kycSvc,
		CashfreeSecret:              cfg.CashfreeWebhookSecret,
		CashfreeEnv:                 cfg.CashfreeEnv,
		CashfreeClient:              cfClient,
		AuthTenantRepo:              tenantRepo,
		AuthUserRepo:                userRepo,
		OutboxEvents:                outboxRepo,
		NotificationSvc:             notifSvc,
		Events:                      pub,
		MagicLinkBaseURL:            cfg.MagicLinkBaseURL,
		VAPIDPublicKey:              cfg.VAPIDPublicKey,
		CORSAllowedOrigins:          cfg.CORSAllowedOrigins,
		TrustedProxies:              cfg.TrustedProxies,
		FrontendURL:                 cfg.FrontendURL,
		AppEnv:                      cfg.AppEnv,
		SearchSvc:                   searchSvc,
		Finance:                     financeSvc,
		ROI:                         roiSvc,
		Intelligence:                intelSvc,
		FinanceEnabled:              cfg.FinanceEnabled,
		IntelligenceEnabled:         cfg.IntelligenceEnabled,
		PayoutRepo:                  payoutRepo,
		PayoutDispatcher:            payoutDispatcher,
		PayoutChecksumSecret:        cfg.PayoutExportChecksumSecret,
		PayoutEncryptionKey:         payoutEncryptionKey,
		CashfreePayoutWebhookSecret: cfg.CashfreePayoutWebhookSecret,
		AttendanceRepo:              attendanceRepo,
		AttendanceSvc:               attendanceSvc,
		BankTxnRepo:                 bankTxnRepo,
		BankAccountRepo:             bankAccountRepo,
		SettlementRepo:              settlementRepo,
		SettlementReconciler:        settlementReconciler,
		SettlementBalancerRepo:      settlementBalancerRepo,
		SettlementBalancer:          settlementBalancer,
		LedgerOutboxRepo:            ledgerOutboxRepo,
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		logger.Info("listening", "addr", cfg.HTTPAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	// Notification dispatcher: polls outbox in the background.
	dispatchCtx, stopDispatcher := context.WithCancel(ctx)
	go dispatcher.Run(dispatchCtx)

	// Daily cleanup: prune dispatched events >30 days, dead-letter >90 days.
	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-dispatchCtx.Done():
				return
			case <-ticker.C:
				n, err := outboxRepo.CleanupEvents(dispatchCtx, 30*24*time.Hour, 90*24*time.Hour)
				if err != nil {
					logger.Error("outbox cleanup failed", "err", err)
				} else {
					logger.Info("outbox cleanup complete", "deleted", n)
				}
				if pn, err := refreshRepo.PurgeExpiredTokens(dispatchCtx, 24*time.Hour); err != nil {
					logger.Error("refresh token purge failed", "err", err)
				} else if pn > 0 {
					logger.Info("refresh token purge complete", "deleted", pn)
				}
			}
		}
	}()

	// Financial Ledger Mirror Outbox Worker: polls pending departure mirror events on a 30s ticker.
	if cfg.FinanceEnabled {
		deadLetterAlerter := finance.NewEmailDeadLetterNotifier(mail, cfg.AdminEmail)
		if cfg.AdminPhone != "" && gateway != nil {
			deadLetterAlerter.WithSMSBackstop(gateway, cfg.AdminPhone)
		}
		ledgerWorker := finance.NewLedgerOutboxWorker(pool, ledgerOutboxRepo, financeSvc, deadLetterAlerter)
		if payoutDispatcher != nil {
			ledgerWorker.SetPayoutDispatcher(payoutDispatcher)
		}

		go func() {
			ticker := time.NewTicker(30 * time.Second)
			defer ticker.Stop()
			for {
				select {
				case <-dispatchCtx.Done():
					return
				case <-ticker.C:
					processed, err := ledgerWorker.ProcessBatch(dispatchCtx, 50)
					if err != nil && !errors.Is(err, context.Canceled) {
						logger.Error("ledger outbox worker batch failed", "err", err)
					} else if processed > 0 {
						logger.Info("ledger outbox worker processed events", "count", processed)
					}
				}
			}
		}()
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	stopDispatcher() // graceful dispatcher shutdown before HTTP drain
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}
