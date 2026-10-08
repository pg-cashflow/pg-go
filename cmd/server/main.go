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
	"github.com/jackc/pgx/v5/pgxpool"
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
	maintPool, err := postgres.NewPool(ctx, cfg.DatabaseMaintURL)
	if err != nil {
		log.Fatal("maint db: ", err)
	}
	defer maintPool.Close()

	var appPool *pgxpool.Pool
	if cfg.DatabaseURL == cfg.DatabaseMaintURL {
		appPool = maintPool
	} else {
		p, err := postgres.NewPool(ctx, cfg.DatabaseURL)
		if err != nil {
			log.Fatal("app db: ", err)
		}
		defer p.Close()
		appPool = p
	}

	// Hardened startup guard: enforces role boundaries, superuser/bypassrls restrictions,
	// and URL separation for production and non-production environments.
	if err := postgres.ValidateStartupRoles(ctx, maintPool, appPool, cfg.DatabaseMaintURL, cfg.DatabaseURL, cfg.AppEnv); err != nil {
		log.Fatal("startup refused: ", err)
	}

	if os.Getenv("AUTO_MIGRATE") == "1" || os.Getenv("AUTO_MIGRATE") == "true" {
		dir := os.Getenv("MIGRATIONS_DIR")
		if dir == "" {
			dir = "migrations"
			if _, err := os.Stat(dir); err != nil {
				dir = filepath.Join("..", "..", "migrations")
			}
		}
		if err := postgres.Migrate(ctx, maintPool, dir); err != nil {
			log.Fatal("migrate: ", err)
		}
	}

	logger := slog.Default()

	scopedDB := postgres.NewScopedDB(appPool)

	propertyRepo := postgres.NewPropertyRepo(maintPool)
	tenantRepo := postgres.NewTenantRepo(scopedDB)
	dueRepo := postgres.NewDueRepo(scopedDB)
	paymentRepo := postgres.NewPaymentRepo(scopedDB)
	eventRepo := postgres.NewEventRepo(scopedDB)
	userRepo := postgres.NewUserRepo(maintPool)
	otpRepo := postgres.NewOTPRepo(maintPool)
	tokenRepo := postgres.NewTokenRepo(maintPool)
	pushRepo := postgres.NewPushRepo(maintPool)
	importRepo := postgres.NewImportRepo(scopedDB)
	joinRepo := postgres.NewJoinRepo(maintPool)
	reportRepo := postgres.NewPaymentReportRepo(scopedDB)
	intentRepo := postgres.NewPaymentIntentRepo(scopedDB)
	preferencesRepo := postgres.NewPreferencesRepo(scopedDB)

	maintTenantRepo := postgres.NewTenantRepo(maintPool)
	maintDueRepo := postgres.NewDueRepo(maintPool)
	maintPaymentRepo := postgres.NewPaymentRepo(maintPool)
	maintReportRepo := postgres.NewPaymentReportRepo(maintPool)
	maintEventRepo := postgres.NewEventRepo(maintPool)

	if cfg.AadhaarQRPublicKeyPEM != "" {
		if err := aadhaar.SetSecureQRPublicKeyPEM(cfg.AadhaarQRPublicKeyPEM); err != nil {
			log.Fatal("aadhaar public key: ", err)
		}
	}

	gamificationRepo := postgres.NewGamificationRepo(maintPool)

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

	refreshRepo := postgres.NewRefreshTokenRepo(maintPool)
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
	billingSvc := billing.NewServiceWithPool(maintPool, maintDueRepo, maintTenantRepo, maintEventRepo)
	paySvc := payment.NewServiceWithPool(maintPool, maintDueRepo, maintPaymentRepo, maintTenantRepo, maintEventRepo, payment.NewSQLSummaryRepository(maintPool))
	depositSettlementRepo := postgres.NewDepositSettlementRepo(maintPool)
	paySvc.SetDepositSettler(depositSettlementRepo)
	tenantSvc := tenant.NewServiceWithPool(maintPool, maintTenantRepo, maintDueRepo, maintEventRepo, pushRepo)
	magicSvc := magiclink.NewService(tokenRepo, maintDueRepo, propertyRepo, maintTenantRepo, cfg.MagicLinkHMACSecret)
	pushSvc := push.NewService(push.RepoAdapter{Inner: pushRepo}, push.Config{
		VAPIDPublicKey:  cfg.VAPIDPublicKey,
		VAPIDPrivateKey: cfg.VAPIDPrivateKey,
		Subject:         cfg.VAPIDSubject,
	}, logger)

	joinSvc := joinsvc.NewServiceWithPool(maintPool, propertyRepo, joinRepo, userRepo, tenantSvc, maintEventRepo)

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
		kycRepo := postgres.NewKYCRepo(maintPool)
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

	financeRepo := postgres.NewFinanceRepo(maintPool)
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
			if err := financeSvc.MirrorProration(ctx, due, original, prorated); err != nil {
				logger.Error("LEDGER GAP: MirrorProration failed", "due_id", due.ID, "err", err)
			}
		})
		billingSvc.SetApplyCreditHook(func(ctx context.Context, propertyID, dueID uuid.UUID, amountPaise int64, dueKind domain.DueKind, at time.Time) {
			if err := financeSvc.MirrorApplyCredit(ctx, propertyID, dueID, amountPaise, dueKind, at); err != nil {
				logger.Error("LEDGER GAP: MirrorApplyCredit failed", "due_id", dueID, "err", err)
			}
		})
		gamificationSvc.SetFinanceHooks(
			func(ctx context.Context, tenant *domain.Tenant, entry *domain.PointsLedgerEntry, pointValuePaise int64) {
				id := uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("pts:%d", entry.ID)))
				amt := int64(entry.Delta) * pointValuePaise
				if err := financeSvc.MirrorPointsIssued(ctx, tenant.PropertyID, tenant.ID, id, entry.Delta, amt); err != nil {
					logger.Error("LEDGER GAP: MirrorPointsIssued failed", "tenant_id", tenant.ID, "err", err)
				}
			},
			func(ctx context.Context, tenant *domain.Tenant, red *domain.Redemption, amountPaise int64) {
				if err := financeSvc.MirrorRewardRedeem(ctx, tenant.PropertyID, tenant.ID, red.ID, red.PointsSpent, amountPaise); err != nil {
					logger.Error("LEDGER GAP: MirrorRewardRedeem failed", "redemption_id", red.ID, "err", err)
				}
			},
		)
	}

	// Notification subsystem.
	outboxRepo := postgres.NewOutboxRepo(maintPool)
	notifRepo := postgres.NewNotificationRepo(maintPool)
	notifSvc := notificationsvc.NewService(notifRepo)
	resolver := notificationsvc.NewResolver(userRepo)
	dispatcher := notificationsvc.NewDispatcher(
		maintPool,
		&notificationsvc.CompositeRepo{OutboxRepo: outboxRepo, NotifRepo: notifRepo},
		resolver,
		notificationsvc.DefaultConfig(),
		logger,
	)

	searchPool, err := postgres.NewSearchPool(ctx, cfg.DatabaseMaintURL)
	if err != nil {
		logger.Warn("failed to initialize dedicated search pool, falling back to maint pool", "err", err)
		searchPool = maintPool
	} else {
		defer searchPool.Close()
	}
	searchRepo := postgres.NewSearchRepo(searchPool)
	// ADR-012: vector/hybrid search permanently disabled.
	searchSvc := &search.Service{Repo: searchRepo, Cache: search.NewCache(5*time.Second, 2000)}

	payoutRepo := postgres.NewPayoutRepo(maintPool, financeSvc)
	attendanceRepo := postgres.NewAttendanceRepo(maintPool)
	attendanceSvc := attendance.NewService(maintPool, attendanceRepo, payoutRepo)
	bankTxnRepo := postgres.NewBankTransactionRepo(maintPool)
	bankAccountRepo := postgres.NewBankAccountRepo(maintPool)
	settlementRepo := postgres.NewSettlementRepo(maintPool)
	settlementReconciler := finance.NewSettlementReconciler(settlementRepo, intentRepo, maintDueRepo, maintPaymentRepo, financeSvc)
	settlementBalancerRepo := postgres.NewSettlementBalancerRepo(maintPool)
	settlementBalancer := finance.NewSettlementBalancer(settlementBalancerRepo)
	ledgerOutboxRepo := postgres.NewLedgerOutboxRepo(maintPool)

	payoutSecret := cfg.PayoutEncryptionSecret
	if payoutSecret == "" {
		payoutSecret = cfg.PayoutExportChecksumSecret
		if payoutSecret == "" {
			payoutSecret = cfg.JWTSecret
		}
	}
	payoutEncryptionKey := crypto.DeriveKey(payoutSecret)
	payoutKeyRing, _ := crypto.NewKeyRing(crypto.CurrentKeyVersion, payoutEncryptionKey)
	for ver, sec := range cfg.PayoutEncryptionKeys {
		_ = payoutKeyRing.AddKey(ver, crypto.DeriveKey(sec))
	}

	var payoutDispatcher *finance.PayoutDispatcher
	if cfg.CashfreePayoutAutoDispatchEnabled && cfg.CashfreePayoutClientID != "" && cfg.CashfreePayoutClientSecret != "" {
		payoutClient := cashfree.NewPayoutClient(cashfree.PayoutConfig{
			ClientID:     cfg.CashfreePayoutClientID,
			ClientSecret: cfg.CashfreePayoutClientSecret,
			APIVersion:   cfg.CashfreePayoutAPIVersion,
			Env:          cfg.CashfreePayoutEnv,
		})
		payoutDispatcher = finance.NewPayoutDispatcher(payoutRepo, payoutClient, cfg.CashfreePayoutFundsourceID, payoutEncryptionKey)
		payoutDispatcher.SetKeyRing(payoutKeyRing)
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
		PaymentLookup:               maintPaymentRepo,
		GatewayPaymentRepo:          maintPaymentRepo,
		WebhookToleranceSec:         cfg.WebhookTimestampToleranceSec,
		WebhookAPIVersion:           cfg.WebhookAPIVersion,
		Pool:                        maintPool,
		Collector:                   collectorSvc,
		KYCSvc:                      kycSvc,
		CashfreeSecret:              cfg.CashfreeWebhookSecret,
		CashfreeEnv:                 cfg.CashfreeEnv,
		CashfreeClient:              cfClient,
		AuthTenantRepo:              maintTenantRepo,
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
		PayoutKeyRing:               payoutKeyRing,
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

	// Daily cleanup: prune dispatched events >30 days, dead-letter >90 days, report proof images >30 days.
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
				if in, err := maintReportRepo.PurgeExpiredImages(dispatchCtx, 30*24*time.Hour); err != nil {
					logger.Error("report image purge failed", "err", err)
				} else if in > 0 {
					logger.Info("report image purge complete", "deleted", in)
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
		ledgerWorker := finance.NewLedgerOutboxWorker(maintPool, ledgerOutboxRepo, financeSvc, deadLetterAlerter)
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
