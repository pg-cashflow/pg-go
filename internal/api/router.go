package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/collector"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/gamification"
	"github.com/pg-cashflow/pg-go/internal/intelligence"
	"github.com/pg-cashflow/pg-go/internal/localization"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/roi"
	"github.com/pg-cashflow/pg-go/internal/search"
	"github.com/pg-cashflow/pg-go/internal/web"
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

	PropertyStore PropertyStore
	TenantStore   TenantStore
	DueStore      DueStore
	PaymentStore  PaymentStore
	EventStore    EventStore
	ImportStore   ImportStore
	UserStore        UserStore
	PreferencesStore PreferencesStore

	Joins          JoinService
	ReportStore    ReportStore
	IntentStore    IntentStore
	PaymentLookup  PaymentLookup
	Collector      *collector.Service
	CashfreeSecret string
	CashfreeEnv    string
	CashfreeClient CashfreeClient

	GatewayPaymentRepo  GatewayPaymentRepo
	WebhookToleranceSec int
	WebhookAPIVersion   string
	Pool                *pgxpool.Pool

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
	FrontendURL        string
	AppEnv             string

	SearchSvc *search.Service

	Finance              *finance.Service
	ROI                  *roi.Service
	Intelligence         *intelligence.Service
	FinanceEnabled       bool
	IntelligenceEnabled  bool

	PayoutRepo                  *postgres.PayoutRepo
	PayoutChecksumSecret        string
	CashfreePayoutWebhookSecret string
	PayoutDispatcher            *finance.PayoutDispatcher

	KYCSvc KYCService // nil-safe: KYC routes 503 gracefully if unset
}

// redactingLogFormatter is a gin log formatter that replaces /p/<token> paths
// with /p/[REDACTED] to prevent 72-hour magic-link credentials appearing in
// server logs or any downstream log aggregator. (ADR-2, M3)
var redactingLogFormatter = func(p gin.LogFormatterParams) string {
	path := p.Path
	if len(path) > 3 && path[:3] == "/p/" {
		path = "/p/[REDACTED]"
	}
	return fmt.Sprintf("[GIN] %s | %3d | %13v | %s\n",
		p.TimeStamp.Format("2006/01/02 - 15:04:05"),
		p.StatusCode,
		p.Latency,
		path,
	)
}

// NewRouter wires all Rev 6 routes.
func NewRouter(d Deps) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery(), gin.LoggerWithConfig(gin.LoggerConfig{
		Formatter: redactingLogFormatter,
	}), localization.Middleware())
	if len(d.CORSAllowedOrigins) > 0 {
		r.Use(cors.New(cors.Config{
			AllowOrigins:     d.CORSAllowedOrigins,
			AllowMethods:     []string{"GET", "POST", "PATCH", "OPTIONS"},
			AllowHeaders:     []string{"Authorization", "Content-Type", "Idempotency-Key", "Accept-Language"},
			AllowCredentials: false,
			MaxAge:           12 * time.Hour,
		}))
	}

	h := &Handlers{Deps: d}

	// Root / Unauthenticated / Standalone HTML
	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/p/:token", h.PaymentPage)
	r.POST("/p/:token/push/subscribe", h.PaymentPushSubscribe)
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)
	r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)

	// Data API routes (Namespaced under /api to avoid SPA collisions)
	api := r.Group("/api")
	{
		api.GET("/healthz", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"status": "ok"})
		})
		api.GET("/push/vapid-public-key", func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"public_key": d.VAPIDPublicKey})
		})
		api.GET("/join/invite/:code", h.LookupInvite)

		// Per-IP rate limits (ADR-2, M2): 3/min OTP, 10/min Firebase (burst headroom for login retries)
		api.POST("/auth/otp/request", ipRateLimit(3.0/60, 5), h.OTPRequest)
		api.POST("/auth/otp/verify", h.OTPVerify)
		api.POST("/auth/firebase", ipRateLimit(10.0/60, 15), h.FirebaseAuth)
		api.POST("/auth/revoke-sessions", auth.RequireOwnerOrManagerOrTenant(d.JWTSecret, d.AuthUserRepo), h.RevokeSessions)

		pending := api.Group("/join", auth.RequirePendingJoin(d.JWTSecret, d.AuthUserRepo))
		{
			pending.GET("/me", h.JoinMe)
			pending.POST("", h.JoinProfile)
		}

		owner := api.Group("/owner", auth.RequireOwner(d.JWTSecret, d.AuthUserRepo))
		{
			owner.GET("/properties", h.ListProperties)
			owner.GET("/invite", h.OwnerInvite)
			owner.POST("/invite/rotate", h.OwnerRotateInvite)
			owner.GET("/join-requests", h.ListJoinRequests)
			owner.POST("/join-requests/:id/activate", h.ActivateJoin)
			owner.POST("/join-requests/:id/reject", h.RejectJoin)

			owner.POST("/tenants", h.CreateTenant)
			owner.GET("/tenants", h.ListTenants)
			owner.PATCH("/tenants/:id", h.UpdateTenant)
			owner.POST("/tenants/:id/notice", h.TenantNotice)
			owner.POST("/tenants/:id/vacate", h.TenantVacate)
			owner.POST("/tenants/:id/attach-phone", h.TenantAttachPhone)
			owner.POST("/tenants/:id/prorate", h.TenantProrate)
			owner.POST("/tenants/:id/deposit/settle", h.TenantDepositSettle)
			owner.GET("/tenants/:id/id-photo", h.TenantIDPhoto)

			owner.GET("/dues", h.ListDues)
			owner.POST("/dues/:id/waive", h.WaiveDue)
			owner.POST("/dues/:id/match", h.ManualMatch)
			owner.POST("/dues/:id/mark-cash-paid", h.MarkCashPaid)
			owner.GET("/dues/:id/qr", h.DueQR)
			owner.GET("/dues/:id/pay", h.OwnerDuePay)
			owner.POST("/dues/:id/token", h.DueToken)

			owner.GET("/payment-reports", h.ListPaymentReports)
			owner.POST("/payment-reports/:id/confirm", h.ConfirmPaymentReport)
			owner.POST("/payment-reports/:id/reject", h.RejectPaymentReport)

			owner.POST("/statements/import", h.ImportStatements)
			owner.GET("/payments", h.ListPayments)
			owner.POST("/payments/:id/refund", h.OwnerRefundPayment)
			owner.GET("/events", h.ListEvents)
			owner.GET("/reconciliation", h.Reconciliation)

			owner.GET("/finance/summary", h.FinanceSummary)
			owner.GET("/finance/ledger", h.FinanceLedger)
			owner.POST("/finance/capital", h.PostCapital)
			owner.GET("/finance/capital", h.ListCapital)
			owner.POST("/finance/expenses", h.PostExpense)
			owner.GET("/finance/expenses", h.ListExpenses)
			owner.POST("/finance/expenses/:id/payments", h.PostExpensePayment)
			owner.GET("/finance/advances", h.ListAdvances)
			owner.POST("/finance/reimbursements", h.PostReimbursement)
			owner.GET("/finance/budgets", h.ListBudgets)
			owner.POST("/finance/budgets", h.PostBudget)
			owner.PATCH("/finance/budgets/:id", h.PatchBudget)
			owner.GET("/finance/settings", h.GetFinanceSettings)
			owner.PATCH("/finance/settings", h.PatchFinanceSettings)
			owner.GET("/finance/policies", h.GetFinanceSettings)
			owner.PATCH("/finance/policies", h.PatchFinanceSettings)
			owner.GET("/finance/approvals", h.ListFinanceApprovals)
			owner.POST("/finance/approvals/:id/:action", h.DecideFinanceApproval)
			owner.GET("/finance/tie-out", h.FinanceTieOut)
			owner.POST("/finance/tie-out/close", h.CloseFinanceTieOut)
			owner.GET("/finance/variance-bridge", h.FinanceVarianceBridge)
			owner.GET("/finance/imports", h.ListExpenseImports)

			owner.GET("/roi", h.OwnerROI)
			owner.GET("/roi/recovery", h.OwnerROIRecovery)
			owner.GET("/roi/break-even", h.OwnerROIBreakEven)
			owner.POST("/roi/scenario", h.OwnerROIScenario)

			owner.GET("/insights", h.InsightsSummary)
			owner.GET("/leakage", h.ListLeakage)
			owner.GET("/leakage/:id", h.GetLeakage)
			owner.GET("/recommendations", h.ListRecommendations)
			owner.POST("/recommendations/:id/:action", h.RecommendationAction)
			owner.GET("/coi", h.OwnerCOI)
			owner.GET("/forecast", h.OwnerForecast)

			// Gamification settings & management
			owner.GET("/gamification/settings", h.OwnerGetGamificationSettings)
			owner.PATCH("/gamification/settings", h.OwnerUpdateGamificationSettings)
			owner.GET("/floors", h.OwnerListFloors)
			owner.POST("/floors", h.OwnerCreateFloor)
			owner.GET("/rooms", h.OwnerListRooms)
			owner.POST("/rooms", h.OwnerCreateRoom)
			owner.POST("/managers", h.OwnerCreateManager)

			// KYC owner review endpoints (ADR-004)
			owner.GET("/tenants/:id/kyc", h.OwnerGetTenantKYC)
			owner.GET("/tenants/:id/kyc/photo", h.OwnerTenantKYCPhoto)
			owner.POST("/tenants/:id/kyc/clear-duplicate", h.OwnerClearDuplicateKYC)

			// Tenant Departures (ADR-009)
			owner.POST("/tenants/:id/departures", h.OwnerCreateDeparture)
			owner.POST("/departures/:id/inspect", h.OwnerInspectDeparture)
			owner.POST("/departures/:id/deductions", h.OwnerAddDepartureDeduction)
			owner.POST("/departures/:id/settle", h.OwnerSettleDeparture)

			// Operational Payouts Subsystem (ADR-009)
			owner.POST("/payouts/payees", h.OwnerCreatePayee)
			owner.GET("/payouts/payees", h.OwnerListPayees)
			owner.GET("/payouts/items/unbatched", h.OwnerListUnbatchedPayoutItems)
			owner.POST("/payouts/batches", h.OwnerCreatePayoutBatch)
			owner.POST("/payouts/batches/:id/approve", h.OwnerApprovePayoutBatch)
			owner.POST("/payouts/batches/:id/approve/request-otp", h.OwnerRequestPayoutBatchOTP)
			owner.GET("/payouts/batches/:id/export", h.OwnerExportPayoutBatch)
			owner.POST("/payouts/batches/:id/dispatch", h.OwnerDispatchPayoutBatch)
		}

		manager := api.Group("/manager", auth.RequireManagerOrOwner(d.JWTSecret, d.AuthUserRepo))
		{
			manager.POST("/inspections", h.ManagerSubmitInspection)
			manager.GET("/inspections", h.ManagerListInspections)
			manager.POST("/inspections/items/:id/resolve", h.ManagerResolveInspectionItem)
			manager.POST("/violations", h.ManagerLogViolation)
			manager.POST("/meter-readings", h.ManagerRecordMeterReading)
			manager.GET("/kitchen/headcount", h.ManagerKitchenHeadcount)
			manager.GET("/hazards", h.ManagerListHazards)
			manager.POST("/hazards/:id/resolve", h.ManagerResolveHazard)
			manager.POST("/vendor-inspections", h.ManagerSubmitVendorInspection)
			manager.GET("/finance/expenses", h.ListExpenses)
			manager.POST("/finance/expenses", h.PostExpense)
			manager.POST("/finance/expenses/:id/payments", h.PostExpensePayment)
			manager.GET("/finance/today", h.ManagerFinanceToday)
			manager.POST("/finance/meal-prep", h.PostMealPrep)
		}

		tenant := api.Group("/tenant", auth.RequireTenant(d.JWTSecret, d.AuthTenantRepo, d.AuthUserRepo))
		{
			tenant.GET("/me", h.TenantMe)
			tenant.GET("/dues", h.TenantDues)
			tenant.GET("/dues/options", h.TenantDuesOptions)
			tenant.POST("/dues/pay-batch", h.TenantDuePayBatch)
			tenant.GET("/dues/:id/qr", h.TenantDueQR)
			tenant.GET("/dues/:id/pay", h.TenantDuePay)
			tenant.POST("/dues/:id/reports", h.TenantSubmitReport)
			tenant.GET("/payments", h.TenantPayments)
			tenant.POST("/push/subscribe", h.TenantPushSubscribe)
			tenant.POST("/aadhaar", h.TenantAadhaar)

			// KYC / Aadhaar identity verification (ADR-004)
			tenant.POST("/kyc/consent", h.TenantKYCConsent)
			tenant.POST("/kyc/initiate", tenantIDRateLimit(10.0/60, 2), h.TenantKYCInitiate)
			tenant.GET("/kyc/return", tenantIDRateLimit(10.0/60, 3), h.TenantKYCReturn)
			tenant.POST("/kyc/upload", tenantIDRateLimit(5.0/60, 2), h.TenantKYCUpload)
			tenant.POST("/kyc/qr", h.TenantKYCSubmitQR)
			tenant.POST("/kyc/revoke", h.TenantKYCRevoke)
			tenant.GET("/kyc/status", h.TenantKYCStatus)

			// Gamification routes
			tenant.GET("/points", h.TenantPoints)
			tenant.GET("/rewards", h.TenantRewards)
			tenant.POST("/rewards/:id/redeem", h.TenantRedeem)
			tenant.GET("/inspections", h.TenantInspections)
			tenant.POST("/inspections/items/:id/dispute", h.TenantDisputeInspectionItem)
			tenant.GET("/meal-rsvp", h.TenantGetMealRSVP)
			tenant.POST("/meal-rsvp", h.TenantSubmitMealRSVP)
			tenant.GET("/menu-poll", h.TenantGetMenuPoll)
			tenant.POST("/menu-poll/vote", h.TenantVoteMenuPoll)
			tenant.POST("/hazards", h.TenantReportHazard)
			tenant.GET("/violations", h.TenantViolations)
			tenant.GET("/leaderboard", h.TenantLeaderboard)
			tenant.GET("/referrals", h.TenantReferrals)
			tenant.POST("/referrals", h.TenantReferrals)
		}

		api.GET("/search", ipRateLimit(30.0/60, 10), auth.RequireOwnerOrManagerOrTenant(d.JWTSecret, d.AuthUserRepo), h.Search)

		// Public locale registry
		api.GET("/locales", h.ListLocales)

		// User preferences — scoped to claims.UserID
		prefs := api.Group("/me/preferences", auth.RequireOwnerOrManagerOrTenant(d.JWTSecret, d.AuthUserRepo))
		{
			prefs.GET("", h.GetPreferences)
			prefs.PATCH("", h.PatchPreferences)
		}

		// Notifications — scoped to claims.UserID at the repo layer.
		// All roles (owner, manager, tenant) share the same endpoints.
		notifs := api.Group("/notifications", auth.RequireOwnerOrManagerOrTenant(d.JWTSecret, d.AuthUserRepo))
		{
			notifs.GET("", h.ListNotifications)
			notifs.PATCH("/:id/read", h.MarkNotificationRead)
			notifs.PATCH("/read-all", h.MarkAllNotificationsRead)
		}

		// Public unauthenticated endpoints (Cashfree webhooks, etc.)
		pub := api.Group("/public")
		{
			// Cashfree Secure ID completion webhook — no auth, HMAC-verified inside handler.
			pub.POST("/cashfree/kyc/webhook", h.CashfreeKYCWebhook)
		}

	}

	// SPA Catch-All
	r.NoRoute(web.Handler())

	return r

}

// Handlers holds Deps for thin HTTP adapters.
type Handlers struct {
	Deps
}
