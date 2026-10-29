package app

import (
	"context"
	"crypto/rsa"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
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
	"github.com/pg-cashflow/pg-go/internal/slotinvite"
	"github.com/pg-cashflow/pg-go/internal/jobs"
	"github.com/pg-cashflow/pg-go/internal/invite"
	joinsvc "github.com/pg-cashflow/pg-go/internal/join"
	kycsvc "github.com/pg-cashflow/pg-go/internal/kyc"
	"github.com/pg-cashflow/pg-go/internal/magiclink"
	"github.com/pg-cashflow/pg-go/internal/mailer"
	notificationsvc "github.com/pg-cashflow/pg-go/internal/notification"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/push"
	"github.com/pg-cashflow/pg-go/internal/referral"
	"github.com/pg-cashflow/pg-go/internal/waitlist"
	roisvc "github.com/pg-cashflow/pg-go/internal/roi"
	"github.com/pg-cashflow/pg-go/internal/search"
	"github.com/pg-cashflow/pg-go/internal/sms"
	"github.com/pg-cashflow/pg-go/internal/tenant"
)

// Server is the wired HTTP application returned by Build.
type Server struct {
	HTTPAddr      string
	Handler       http.Handler
	RunBackground func(context.Context)
	Cleanup       func()
}

// Build constructs pools, services, and the Gin router for cmd/server.
func Build(ctx context.Context, cfg *config.Config) (*Server, error) {
	logger := slog.Default()

	maintPool, err := postgres.NewPool(ctx, cfg.DatabaseMaintURL)
	if err != nil {
		return nil, fmt.Errorf("maint db: %w", err)
	}

	var appPool *pgxpool.Pool
	appPoolSameAsMaint := cfg.DatabaseURL == cfg.DatabaseMaintURL
	if appPoolSameAsMaint {
		appPool = maintPool
	} else {
		p, err := postgres.NewPool(ctx, cfg.DatabaseURL)
		if err != nil {
			maintPool.Close()
			return nil, fmt.Errorf("app db: %w", err)
		}
		appPool = p
	}

	if err := postgres.ValidateStartupRoles(ctx, maintPool, appPool, cfg.DatabaseMaintURL, cfg.DatabaseURL, cfg.AppEnv); err != nil {
		if !appPoolSameAsMaint {
			appPool.Close()
		}
		maintPool.Close()
		return nil, fmt.Errorf("startup refused: %w", err)
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
			if !appPoolSameAsMaint {
				appPool.Close()
			}
			maintPool.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}

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
	calendarTokenRepo := postgres.NewCalendarTokenRepo(scopedDB)

	maintTenantRepo := postgres.NewTenantRepo(maintPool)
	maintDueRepo := postgres.NewDueRepo(maintPool)
	maintPaymentRepo := postgres.NewPaymentRepo(maintPool)
	maintReportRepo := postgres.NewPaymentReportRepo(maintPool)

	if cfg.AadhaarQRPublicKeyPEM != "" {
		if err := aadhaar.SetSecureQRPublicKeyPEM(cfg.AadhaarQRPublicKeyPEM); err != nil {
			return nil, fmt.Errorf("aadhaar public key: %w", err)
		}
	}

	gamificationRepo := postgres.NewGamificationRepo(maintPool)

	innerPub := events.NewPostgresPublisher(eventRepo)
	dispatchPub := events.NewDispatchPublisher(innerPub)
	pub := dispatchPub

	channels := OpenChannels(cfg)
	mail := channels.Mail
	gateway := channels.Gateway

	refreshRepo := postgres.NewRefreshTokenRepo(maintPool)
	authSvc := auth.NewService(otpRepo, userRepo, tenantRepo, propertyRepo, gateway, cfg.OTPHMACSecret, cfg.JWTSecret)
	authSvc.SetMailer(mail)
	authSvc.SetRefreshTokenRepo(refreshRepo)
	authSvc.SetPhoneLimiter(api.DefaultFirebasePhoneLimiter)
	if cfg.FirebaseProjectID != "" {
		if cfg.FirebaseCredentials != "" {
			if _, err := os.Stat(cfg.FirebaseCredentials); err != nil {
				logger.Error("firebase credentials file missing", "path", cfg.FirebaseCredentials, "err", err)
			}
		}
		fbVerifier, err := auth.NewFirebaseVerifier(ctx, cfg.FirebaseProjectID, cfg.FirebaseCredentials)
		if err != nil {
			if cfg.AppEnv == "production" {
				return nil, fmt.Errorf("firebase: %w", err)
			}
			logger.Error("firebase auth disabled", "err", err, "credentials", cfg.FirebaseCredentials)
		} else {
			authSvc.SetFirebaseVerifier(fbVerifier)
			logger.Info("firebase phone auth enabled", "project", cfg.FirebaseProjectID)
		}
	}
	billingSvc := billing.NewServiceWithPool(appPool, dueRepo, tenantRepo, eventRepo)
	paySvc := payment.NewServiceWithPool(appPool, dueRepo, paymentRepo, tenantRepo, eventRepo, payment.NewSQLSummaryRepository(scopedDB))
	depositSettlementRepo := postgres.NewDepositSettlementRepo(appPool)
	paySvc.SetDepositSettler(depositSettlementRepo)
	tenantSvc := tenant.NewServiceWithPool(appPool, tenantRepo, dueRepo, eventRepo, pushRepo)
	magicSvc := magiclink.NewService(tokenRepo, maintDueRepo, propertyRepo, maintTenantRepo, cfg.MagicLinkHMACSecret)
	pushSvc := push.NewService(push.RepoAdapter{Inner: pushRepo}, push.Config{
		VAPIDPublicKey:  cfg.VAPIDPublicKey,
		VAPIDPrivateKey: cfg.VAPIDPrivateKey,
		Subject:         cfg.VAPIDSubject,
	}, logger)

	slotInviteRepo := postgres.NewSlotInviteRepo(maintPool)
	onboardingSettingsRepo := postgres.NewOnboardingSettingsRepo(maintPool)
	inviteResolver := invite.NewResolver(slotInviteRepo)
	authSvc.SetInviteResolver(inviteResolver)
	slotInviteSvc := slotinvite.NewService(maintPool, slotInviteRepo, onboardingSettingsRepo)

	joinIdempotencyRepo := postgres.NewJoinIdempotencyRepo(maintPool)
	joinSvc := joinsvc.NewServiceWithPool(appPool, propertyRepo, joinRepo, userRepo, tenantSvc, eventRepo, inviteResolver, slotInviteRepo, onboardingSettingsRepo)

	referralRepo := postgres.NewReferralRepo(maintPool)
	referralSvc := referral.NewService(referralRepo)
	joinSvc.SetReferralBinder(referralSvc)
	billingSvc.SetFirstRentReferralHooks(referralSvc.ApplyFirstRentDiscount, referralSvc.RecordFirstRentAdjustment)
	referralConsumer := referral.NewEventConsumer(referralSvc, logger)
	dispatchPub.Subscribe(referralConsumer.ProcessEventAsync)

	waitlistRepo := postgres.NewWaitlistRepo(maintPool)
	waitlistSvc := waitlist.NewService(maintPool, waitlistRepo, slotInviteRepo, onboardingSettingsRepo)
	waitlistConsumer := waitlist.NewEventConsumer(waitlistSvc, maintTenantRepo, logger)
	dispatchPub.Subscribe(waitlistConsumer.ProcessEventAsync)

	var cfOrders collector.CashfreeOrders
	var cfClient *cashfree.Client
	cfCfg := cashfree.Config{AppID: cfg.CashfreeAppID, SecretKey: cfg.CashfreeSecretKey, Env: cfg.CashfreeEnv}
	if strings.TrimSpace(cfg.FrontendURL) != "" {
		cfCfg.ReturnURL = strings.TrimRight(strings.TrimSpace(cfg.FrontendURL), "/") + "/tenant/pay-return?order_id={order_id}"
	} else {
		baseURL := "http://localhost:8080"
		if addr := strings.TrimSpace(cfg.HTTPAddr); addr != "" {
			if strings.HasPrefix(addr, ":") {
				baseURL = "http://localhost" + addr
			} else if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
				baseURL = strings.TrimRight(addr, "/")
			} else {
				baseURL = "http://" + addr
			}
		}
		cfCfg.ReturnURL = baseURL + "/tenant/pay-return?order_id={order_id}"
	}
	if cfCfg.Enabled() {
		cfClient = cashfree.NewClient(cfCfg)
		if cfg.CashfreePGBaseURL != "" {
			cfClient.WithBaseURL(cfg.CashfreePGBaseURL)
			logger.Warn("cashfree pg base url overridden", "url", cfg.CashfreePGBaseURL)
		}
		cfOrders = cfClient
		logger.Info("cashfree orders enabled", "env", cfg.CashfreeEnv)
	}
	collectorSvc := collector.New(intentRepo, cfOrders)

	var kycSvc api.KYCService
	if cfg.KYCIdentitySecret != "" {
		// Decode AES-256-GCM key for Aadhaar document-bytes encryption.
		// KYC_DOC_CRYPTO_KEY must be exactly 64 hex chars (= 32 bytes).
		//
		// Security invariants (both enforced in KYCRepo.encryptBytes / decryptBytes):
		//   - Absent key (empty string): encryption intentionally disabled â€” documents stored as
		//     plaintext. This is the opted-out path; warn only.
		//   - Present but malformed key: FATAL â€” the repo hard-errors on any encrypt/decrypt call
		//     rather than silently storing Aadhaar documents in plaintext. Fail at startup here to
		//     surface this misconfiguration immediately.
		var kycDocKey []byte
		if hex64 := cfg.KYCDocCryptoKey; hex64 != "" {
			decoded, decErr := hex.DecodeString(hex64)
			if decErr != nil || len(decoded) != 32 {
				// A present-but-malformed key must never silently fall through to plaintext.
				return nil, fmt.Errorf("kyc: KYC_DOC_CRYPTO_KEY is set but invalid (must be 64 hex chars / 32 bytes); "+
					"refusing to start to avoid silently storing Aadhaar documents unencrypted: %v", decErr)
			}
			kycDocKey = decoded
			logger.Info("kyc: document-bytes encryption enabled")
		} else {
			if cfg.AppEnv == "production" {
				return nil, fmt.Errorf("kyc: KYC_DOC_CRYPTO_KEY is mandatory in production (DPDP Â§8(5) requirement)")
			}
			logger.Warn("kyc: KYC_DOC_CRYPTO_KEY not set â€” Aadhaar document bytes will be stored unencrypted (DPDP Â§8(5) gap)")
		}
		kycRepo := postgres.NewKYCRepo(maintPool, kycDocKey)
		if len(kycDocKey) == 32 {
			kycKeyRing, err := crypto.NewKeyRing(crypto.CurrentKeyVersion, kycDocKey)
			if err != nil {
				return nil, fmt.Errorf("kyc: failed to initialize key ring: %w", err)
			}
			kycRepo.SetKeyRing(kycKeyRing)
		}
		var cfKYCAdapter kycsvc.CashfreeKYCClient
		if cfg.CashfreeKYCAppID != "" && cfg.CashfreeKYCSecretKey != "" {
			cfKYCClient := cashfree.NewClient(cashfree.Config{
				AppID:     cfg.CashfreeKYCAppID,
				SecretKey: cfg.CashfreeKYCSecretKey,
				Env:       cfg.CashfreeEnv,
			})
			cfKYCAdapter = kycsvc.NewCashfreeAdapter(cfKYCClient)
			logger.Info("kyc service enabled with dedicated Cashfree KYC credentials")
		} else if cfClient != nil {
			cfKYCAdapter = kycsvc.NewCashfreeAdapter(cfClient)
			logger.Info("kyc service enabled with default Cashfree credentials")
		} else {
			logger.Warn("kyc service enabled in document-only mode (Cashfree credentials not configured; Aadhaar OTP unavailable)")
		}
		kycSvc = kycsvc.NewService(kycRepo, cfKYCAdapter, kycsvc.Config{
			IdentitySecret:           cfg.KYCIdentitySecret,
			HashKeyVersion:           1,
			VerificationValidityDays: 365,
		})
	} else {
		logger.Info("kyc service dormant (KYC_IDENTITY_SECRET not set)")
	}

	gamificationSvc := gamification.NewService(gamificationRepo, tenantRepo, dueRepo, dispatchPub, gamification.NewBlobStore())
	gamificationConsumer := gamification.NewEventConsumer(gamificationSvc)
	dispatchPub.Subscribe(gamificationConsumer.ProcessEventAsync)

	financeRepo := postgres.NewFinanceRepo(appPool, scopedDB)
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
				id := uuid.NewSHA1(uuid.NameSpaceOID, fmt.Appendf(nil, "pts:%d", entry.ID))
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
	searchPoolOwned := err == nil && searchPool != maintPool
	if err != nil {
		logger.Warn("failed to initialize dedicated search pool, falling back to maint pool", "err", err)
		searchPool = maintPool
	}
	searchRepo := postgres.NewSearchRepo(searchPool)
	searchSvc := &search.Service{Repo: searchRepo, Cache: search.NewCache(5*time.Second, 2000)}

	payoutRepo := postgres.NewPayoutRepo(appPool, scopedDB, financeSvc)
	attendanceRepo := postgres.NewAttendanceRepo(appPool)
	attendanceSvc := attendance.NewService(appPool, attendanceRepo, payoutRepo)
	bankTxnRepo := postgres.NewBankTransactionRepo(scopedDB)
	bankAccountRepo := postgres.NewBankAccountRepo(scopedDB)
	settlementRepo := postgres.NewSettlementRepo(maintPool)
	settlementReconciler := finance.NewSettlementReconciler(settlementRepo, intentRepo, maintDueRepo, maintPaymentRepo, financeSvc)
	settlementBalancerRepo := postgres.NewSettlementBalancerRepo(maintPool)
	settlementBalancer := finance.NewSettlementBalancer(settlementBalancerRepo)
	ledgerOutboxRepo := postgres.NewLedgerOutboxRepo(maintPool)

	payoutSecret := cfg.PayoutEncryptionSecret
	if payoutSecret == "" {
		if cfg.AppEnv == "production" || cfg.CashfreePayoutClientID != "" {
			if !appPoolSameAsMaint {
				appPool.Close()
			}
			maintPool.Close()
			if searchPoolOwned {
				searchPool.Close()
			}
			return nil, errors.New("PAYOUT_ENCRYPTION_SECRET is required when in production or when CF_PAYOUT_CLIENT_ID is set (must not fall back to JWT_SECRET)")
		}
		payoutSecret = cfg.PayoutExportChecksumSecret
		if payoutSecret == "" {
			payoutSecret = cfg.JWTSecret
		}
	}
	payoutEncryptionKey := crypto.DeriveKey(payoutSecret)
	payoutKeyRing, err := crypto.NewKeyRing(crypto.CurrentKeyVersion, payoutEncryptionKey)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize payout key ring: %w", err)
	}
	for ver, sec := range cfg.PayoutEncryptionKeys {
		if err := payoutKeyRing.AddKey(ver, crypto.DeriveKey(sec)); err != nil {
			return nil, fmt.Errorf("failed to add versioned payout key %d: %w", ver, err)
		}
	}

	approvalSecret := cfg.PayoutApprovalSecret
	if approvalSecret == "" {
		approvalSecret = cfg.PayoutExportChecksumSecret
	}
	if approvalSecret == "" {
		approvalSecret = cfg.JWTSecret
	}
	payoutApprovalSigningKey := []byte(approvalSecret)
	approvalSigningKeys := map[string][]byte{
		"v1": payoutApprovalSigningKey,
	}
	for kid, sec := range cfg.PayoutApprovalKeys {
		approvalSigningKeys[kid] = []byte(sec)
	}

	var payoutDispatcher *finance.PayoutDispatcher
	if cfg.CashfreePayoutAutoDispatchEnabled && cfg.CashfreePayoutClientID != "" && cfg.CashfreePayoutClientSecret != "" {
		var pubKey *rsa.PublicKey
		if strings.EqualFold(cfg.CashfreePayoutAuthMode, "signature") && cfg.CashfreePayoutPublicKeyPEM != "" {
			var err error
			pubKey, err = cashfree.ParseRSAPublicKeyOrCertificate([]byte(cfg.CashfreePayoutPublicKeyPEM))
			if err != nil {
				return nil, fmt.Errorf("bootstrap payout public key: %w", err)
			}
		}
		payoutClient := cashfree.NewPayoutClient(cashfree.PayoutConfig{
			ClientID:     cfg.CashfreePayoutClientID,
			ClientSecret: cfg.CashfreePayoutClientSecret,
			APIVersion:   cfg.CashfreePayoutAPIVersion,
			Env:          cfg.CashfreePayoutEnv,
			AuthMode:     cfg.CashfreePayoutAuthMode,
			PublicKey:    pubKey,
		})
		payoutDispatcher = finance.NewPayoutDispatcher(payoutRepo, payoutClient, cfg.CashfreePayoutFundsourceID, payoutEncryptionKey)
		payoutDispatcher.SetKeyRing(payoutKeyRing)
		payoutDispatcher.SetApprovalSigningKeys(approvalSigningKeys)
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
		InviteResolver:              inviteResolver,
		SlotInvites:                 slotInviteSvc,
		Referrals:                   referralSvc,
		Waitlist:                    waitlistSvc,
		OnboardingSettings:          onboardingSettingsRepo,
		JoinIdempotency:             joinIdempotencyRepo,
		ReportStore:                 reportRepo,
		IntentStore:                 intentRepo,
		PaymentLookup:               maintPaymentRepo,
		GatewayPaymentRepo:          maintPaymentRepo,
		WebhookToleranceSec:         cfg.WebhookTimestampToleranceSec,
		WebhookAPIVersion:           cfg.WebhookAPIVersion,
		Pool:                        maintPool,
		AppPool:                     appPool,
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
		PayoutApprovalSigningKey:    payoutApprovalSigningKey,
		PayoutApprovalKeys:          approvalSigningKeys,
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
		MetricsSecret:               cfg.MetricsSecret,
		CapabilityStore:             postgres.NewCapabilityRepo(maintPool),
		CalendarTokenStore:          calendarTokenRepo,
	})

	bg := backgroundConfig{
		logger:          logger,
		maintPool:       maintPool,
		outboxRepo:      outboxRepo,
		refreshRepo:     refreshRepo,
		maintReportRepo: maintReportRepo,
		dispatcher:      dispatcher,
		financeEnabled:  cfg.FinanceEnabled,
		ledgerOutbox:    ledgerOutboxRepo,
		financeSvc:      financeSvc,
		payoutDispatch:  payoutDispatcher,
		mail:            mail,
		adminEmail:      cfg.AdminEmail,
		adminPhone:      cfg.AdminPhone,
		smsGateway:      gateway,
	}

	cleanup := func() {
		if searchPoolOwned {
			searchPool.Close()
		}
		if !appPoolSameAsMaint {
			appPool.Close()
		}
		maintPool.Close()
	}

	return &Server{
		HTTPAddr:      cfg.HTTPAddr,
		Handler:       router,
		RunBackground: func(ctx context.Context) { runBackgroundWorkers(ctx, bg) },
		Cleanup:       cleanup,
	}, nil
}

type backgroundConfig struct {
	logger          *slog.Logger
	maintPool       *pgxpool.Pool
	outboxRepo      *postgres.OutboxRepo
	refreshRepo     *postgres.RefreshTokenRepo
	maintReportRepo *postgres.PaymentReportRepo
	dispatcher      *notificationsvc.Dispatcher
	financeEnabled  bool
	ledgerOutbox    *postgres.LedgerOutboxRepo
	financeSvc      *finance.Service
	payoutDispatch  *finance.PayoutDispatcher
	mail            mailer.Mailer
	adminEmail      string
	adminPhone      string
	smsGateway      sms.SMSGateway
}

func runBackgroundWorkers(ctx context.Context, bg backgroundConfig) {
	go bg.dispatcher.Run(ctx)
	auth.StartCacheSweeper(ctx, 5*time.Minute)

	go func() {
		ticker := time.NewTicker(24 * time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				release, acquired, err := jobs.AcquireJobLock(ctx, bg.maintPool, "daily_cleanup")
				if err != nil {
					bg.logger.Error("daily cleanup lock failed", "err", err)
					continue
				}
				if !acquired {
					bg.logger.Debug("daily cleanup lock held by another instance, skipping")
					continue
				}
				n, err := bg.outboxRepo.CleanupEvents(ctx, 30*24*time.Hour, 90*24*time.Hour)
				if err != nil {
					bg.logger.Error("outbox cleanup failed", "err", err)
				} else {
					bg.logger.Info("outbox cleanup complete", "deleted", n)
				}
				if pn, err := bg.refreshRepo.PurgeExpiredTokens(ctx, 24*time.Hour); err != nil {
					bg.logger.Error("refresh token purge failed", "err", err)
				} else if pn > 0 {
					bg.logger.Info("refresh token purge complete", "deleted", pn)
				}
				if in, err := bg.maintReportRepo.PurgeExpiredImages(ctx, 30*24*time.Hour); err != nil {
					bg.logger.Error("report image purge failed", "err", err)
				} else if in > 0 {
					bg.logger.Info("report image purge complete", "deleted", in)
				}
				_ = release(ctx)
			}
		}
	}()

	if !bg.financeEnabled {
		return
	}

	deadLetterAlerter := finance.NewEmailDeadLetterNotifier(bg.mail, bg.adminEmail)
	if bg.adminPhone != "" && bg.smsGateway != nil {
		deadLetterAlerter.WithSMSBackstop(bg.smsGateway, bg.adminPhone)
	}
	ledgerWorker := finance.NewLedgerOutboxWorker(bg.maintPool, bg.ledgerOutbox, bg.financeSvc, deadLetterAlerter)
	if bg.payoutDispatch != nil {
		ledgerWorker.SetPayoutDispatcher(bg.payoutDispatch)
	}

	if bg.payoutDispatch != nil {
		payoutLister := postgres.NewPayoutRepo(bg.maintPool)
		bg.logger.Info("payout status poller started", "interval", "5m", "stale_after", "10m")
		go func() {
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					release, acquired, err := jobs.AcquireJobLock(ctx, bg.maintPool, "payout_status_poller")
					if err != nil {
						bg.logger.Error("payout poller lock failed", "err", err)
						continue
					}
					if !acquired {
						continue
					}
					n, err := bg.payoutDispatch.PollStaleBatches(ctx, payoutLister, 10*time.Minute, 50)
					_ = release(ctx)
					if err != nil && !errors.Is(err, context.Canceled) {
						bg.logger.Error("payout poller finished with errors", "polled", n, "err", err)
					} else if n > 0 {
						bg.logger.Info("payout poller reconciled batches", "count", n)
					}
				}
			}
		}()
	}

	bg.logger.Info("ledger outbox worker started", "poll_interval", "30s", "batch_size", 50)

	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				release, acquired, err := jobs.AcquireJobLock(ctx, bg.maintPool, "ledger_outbox_worker")
				if err != nil {
					bg.logger.Error("ledger outbox worker lock failed", "err", err)
					continue
				}
				if !acquired {
					continue
				}
				processed, err := ledgerWorker.ProcessBatch(ctx, 50)
				_ = release(ctx)
				if err != nil && !errors.Is(err, context.Canceled) {
					bg.logger.Error("ledger outbox worker batch failed", "err", err)
				} else if processed > 0 {
					bg.logger.Info("ledger outbox worker processed events", "count", processed)
				}
			}
		}
	}()
}
