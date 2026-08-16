package api

import (
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/collector"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/web"
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

	Joins          JoinService
	ReportStore    ReportStore
	IntentStore    IntentStore
	PaymentLookup  PaymentLookup
	Collector      *collector.Service
	CashfreeSecret string
	CashfreeEnv    string

	AuthTenantRepo auth.TenantRepository // for RequireTenant live check

	Events             events.Publisher
	MagicLinkBaseURL   string
	VAPIDPublicKey     string
	CORSAllowedOrigins []string
	FrontendURL        string
	AppEnv             string
}

// NewRouter wires all Rev 6 routes.
func NewRouter(d Deps) *gin.Engine {
	r := gin.New()
	r.Use(gin.Recovery(), gin.Logger())
	if len(d.CORSAllowedOrigins) > 0 {
		r.Use(cors.New(cors.Config{
			AllowOrigins:     d.CORSAllowedOrigins,
			AllowMethods:     []string{"GET", "POST", "PATCH", "OPTIONS"},
			AllowHeaders:     []string{"Authorization", "Content-Type"},
			AllowCredentials: false,
			MaxAge:           12 * time.Hour,
		}))
	}

	r.GET("/healthz", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	r.GET("/push/vapid-public-key", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"public_key": d.VAPIDPublicKey})
	})

	h := &Handlers{Deps: d}

	serveEmbeddedApp := !strings.EqualFold(d.AppEnv, "production") || strings.TrimSpace(d.FrontendURL) == ""
	r.GET("/", func(c *gin.Context) {
		if !serveEmbeddedApp {
			c.Redirect(http.StatusFound, d.FrontendURL)
			return
		}
		c.Redirect(http.StatusFound, "/app/")
	})
	if serveEmbeddedApp {
		r.GET("/app", func(c *gin.Context) { c.Redirect(http.StatusFound, "/app/") })
		if sub, err := fs.Sub(web.FS, "."); err == nil {
			r.GET("/app/*filepath", func(c *gin.Context) {
				p := strings.TrimPrefix(c.Param("filepath"), "/")
				if p == "" {
					p = "index.html"
				}
				data, err := fs.ReadFile(sub, p)
				if err != nil {
					c.Status(http.StatusNotFound)
					return
				}
				switch {
				case strings.HasSuffix(p, ".js"):
					c.Data(http.StatusOK, "text/javascript; charset=utf-8", data)
				case strings.HasSuffix(p, ".css"):
					c.Data(http.StatusOK, "text/css; charset=utf-8", data)
				case strings.HasSuffix(p, ".json"):
					c.Data(http.StatusOK, "application/json", data)
				case strings.HasSuffix(p, ".webmanifest"):
					c.Data(http.StatusOK, "application/manifest+json", data)
				default:
					c.Data(http.StatusOK, "text/html; charset=utf-8", data)
				}
			})
		}
	} else {
		r.GET("/app", func(c *gin.Context) { c.Redirect(http.StatusFound, d.FrontendURL) })
		r.GET("/app/*filepath", func(c *gin.Context) { c.Redirect(http.StatusFound, d.FrontendURL) })
	}

	// Unauthenticated
	r.GET("/p/:token", h.PaymentPage)
	r.POST("/p/:token/push/subscribe", h.PaymentPushSubscribe)
	r.POST("/auth/firebase", h.FirebaseAuth)
	r.GET("/join/invite/:code", h.LookupInvite)
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)

	pending := r.Group("/join", auth.RequirePendingJoin(d.JWTSecret))
	{
		pending.GET("/me", h.JoinMe)
		pending.POST("", h.JoinProfile)
	}

	owner := r.Group("/owner", auth.RequireOwner(d.JWTSecret))
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
		owner.GET("/events", h.ListEvents)
		owner.GET("/reconciliation", h.Reconciliation)
	}

	tenant := r.Group("/tenant", auth.RequireTenant(d.JWTSecret, d.AuthTenantRepo))
	{
		tenant.GET("/me", h.TenantMe)
		tenant.GET("/dues", h.TenantDues)
		tenant.GET("/dues/:id/qr", h.TenantDueQR)
		tenant.GET("/dues/:id/pay", h.TenantDuePay)
		tenant.POST("/dues/:id/reports", h.TenantSubmitReport)
		tenant.GET("/payments", h.TenantPayments)
		tenant.POST("/push/subscribe", h.TenantPushSubscribe)
		tenant.POST("/aadhaar", h.TenantAadhaar)
	}

	return r
}

// Handlers holds Deps for thin HTTP adapters.
type Handlers struct {
	Deps
}
