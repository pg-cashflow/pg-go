package api

import (
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/attendance"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/collector"
	"github.com/pg-cashflow/pg-go/internal/crypto"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/gamification"
	"github.com/pg-cashflow/pg-go/internal/intelligence"
	"github.com/pg-cashflow/pg-go/internal/localization"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/roi"
	"github.com/pg-cashflow/pg-go/internal/search"
	legacypwa "github.com/pg-cashflow/pg-go/web"

	spaweb "github.com/pg-cashflow/pg-go/internal/web"
)

// Deps aggregates services and stores for HTTP handlers.
type Deps struct {
	JWTSecret string

	Auth      AuthService
	MagicLink MagicLinkService
	Push      PushService
	Tenants   TenantService
	Billing   BillingService
	Payments  PaymentService

	PropertyStore    PropertyStore
	TenantStore      TenantStore
	DueStore         DueStore
	PaymentStore     PaymentStore
	EventStore       EventStore
	ImportStore      ImportStore
	UserStore        UserStore
	PreferencesStore PreferencesStore

	Joins          JoinService
	InviteResolver InviteResolver
	SlotInvites          SlotInviteService
	Referrals            ReferralAdmissionService
	Waitlist             WaitlistAdmissionService
	OnboardingSettings   OnboardingSettingsStore
	JoinIdempotency      JoinIdempotencyStore
	ReportStore    ReportStore
	IntentStore    IntentStore
	PaymentLookup  PaymentLookup
	Collector      *collector.Service
	CashfreeSecret string
	CashfreeEnv    string
	CashfreeClient CashfreeClient

	GatewayPaymentRepo  GatewayPaymentRepo
	UnmatchedReceipts   UnmatchedReceiptStore
	WebhookToleranceSec int
	WebhookAPIVersion   string
	Pool                *pgxpool.Pool
	AppPool             *pgxpool.Pool
	DBCluster           *postgres.DBCluster
	RefundTxRunner      RefundTxRunner
	CalendarTokenStore  CalendarTokenStore

	Gamification      *gamification.Service
	GamificationStore gamification.Store

	AuthTenantRepo auth.TenantRepository // for RequireTenant live check
	AuthUserRepo   auth.SessionStore     // for token_version session checks + revoke

	OutboxEvents    OutboxStore         // notification outbox (best-effort from handlers)
	NotificationSvc NotificationService // in-app notification read-side

	Events             events.Publisher
	MagicLinkBaseURL   string
	VAPIDPublicKey     string
	CORSAllowedOrigins []string
	TrustedProxies     []string
	FrontendURL        string
	AppEnv             string

	SearchSvc *search.Service

	Finance             *finance.Service
	ROI                 *roi.Service
	Intelligence        *intelligence.Service
	FinanceEnabled      bool
	IntelligenceEnabled bool

	PayoutRepo                  *postgres.PayoutRepo
	PayoutBatchRepo             PayoutBatchStore
	PayoutWebhookRepo           PayoutWebhookStore
	PayoutChecksumSecret        string
	PayoutApprovalSigningKey    []byte
	PayoutApprovalKeys          map[string][]byte
	PayoutEncryptionKey         []byte
	PayoutKeyRing               *crypto.KeyRing
	CashfreePayoutWebhookSecret string
	PayoutDispatcher            *finance.PayoutDispatcher
	LedgerOutboxRepo            *postgres.LedgerOutboxRepo

	AttendanceRepo *postgres.AttendanceRepo
	AttendanceSvc  *attendance.Service

	SettlementRepo       *postgres.SettlementRepo
	SettlementReconciler SettlementReconcilerService

	SettlementBalancerRepo SettlementBalancerStore
	SettlementBalancer     *finance.SettlementBalancer

	BankTxnRepo BankTransactionStore
	// CapabilityStore enforces ADR-032 grants on /manager routes.
	// Nil skips enforcement so tests that do not set a store keep prior behavior.
	CapabilityStore ManagerCapabilityStore
	BankAccountRepo BankAccountStore

	KYCSvc KYCService // nil-safe: KYC routes 503 gracefully if unset

	// MetricsSecret is the static token accepted in the X-Metrics-Token header.
	// REQ-SEC-003: owner JWTs do not authorize /metrics.
	// REQ-SEC-003: /metrics requires either this token or a valid owner/manager JWT.
	// Must be set to a high-entropy random value in production.
	MetricsSecret string
}

func NewRouter(d Deps) *gin.Engine {
	if (d.AppEnv == "production" || d.AppEnv == "staging") && d.Pool == nil {
		panic("api: database pool is required in production/staging")
	}

	r := gin.New()
	if len(d.TrustedProxies) > 0 {
		_ = r.SetTrustedProxies(d.TrustedProxies)
	} else {
		_ = r.SetTrustedProxies(nil)
	}

	r.Use(
		RequestIDMiddleware(),
		gin.Recovery(),
		CloudflareRealIP(d.TrustedProxies), // must be before any rate-limit or audit that reads c.ClientIP()
		gin.LoggerWithConfig(gin.LoggerConfig{
			Formatter: redactingLogFormatter,
		}),
		SecurityHeaders(d.AppEnv),
		MaxBodyBytes(10<<20),
		localization.Middleware(),
	)
	if len(d.CORSAllowedOrigins) > 0 {
		r.Use(cors.New(cors.Config{
			AllowOrigins:     d.CORSAllowedOrigins,
			AllowMethods:     []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
			AllowHeaders:     []string{"Authorization", "Content-Type", "Idempotency-Key", "X-Idempotency-Key", "Accept-Language", "X-Request-ID", "X-Correlation-ID", "X-Property-ID"},
			AllowCredentials: true,
			MaxAge:           12 * time.Hour,
		}))
	}

	if legacyAppShellEnabled(d) {
		r.Use(legacypwa.Handler())
	}

	if d.UnmatchedReceipts == nil && d.GatewayPaymentRepo != nil {
		if u, ok := d.GatewayPaymentRepo.(UnmatchedReceiptStore); ok {
			d.UnmatchedReceipts = u
		}
	}

	h := &Handlers{Deps: d}
	health := healthHandler()
	registerPublicRoutes(r, d, h, health)
	registerAPIRoutes(r, d, h, health)

	r.NoRoute(spaweb.Handler())

	return r
}

// Handlers holds Deps for thin HTTP adapters.
type Handlers struct {
	Deps
}
