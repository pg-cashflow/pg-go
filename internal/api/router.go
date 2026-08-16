package api

import (
	"net/http"
	"time"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/events"
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

	AuthTenantRepo auth.TenantRepository // for RequireTenant live check

	Events             events.Publisher
	MagicLinkBaseURL   string
	VAPIDPublicKey     string
	CORSAllowedOrigins []string
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

	// Unauthenticated
	r.GET("/p/:token", h.PaymentPage)
	r.POST("/p/:token/push/subscribe", h.PaymentPushSubscribe)
	r.POST("/auth/firebase", h.FirebaseAuth)

	owner := r.Group("/owner", auth.RequireOwner(d.JWTSecret))
	{
		owner.GET("/properties", h.ListProperties)

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
		owner.POST("/dues/:id/token", h.DueToken)

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
