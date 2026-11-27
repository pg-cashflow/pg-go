package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/auth"
)

func registerAPIRoutes(r *gin.Engine, d Deps, h *Handlers, health gin.HandlerFunc) {
	api := r.Group("/api")
	api.GET("/healthz", health)
	api.GET("/healthz/workers", workerHealthHandler(d.WorkerHealth))
	api.GET("/metrics", metricsAuth(d.JWTSecret, d.MetricsSecret), h.Metrics)
	api.GET("/push/vapid-public-key", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"public_key": d.VAPIDPublicKey})
	})
	api.GET("/join/invite/:code", ipRateLimit(60.0/60, 100), h.LookupInvite)
	api.GET("/owner/calendar.ics", h.OwnerCalendarICS)

	// RequireSameOrigin is defense-in-depth behind SameSite=Strict cookies.
	// It rejects browser-initiated cross-origin POSTs without preventing
	// legitimate non-browser clients (native app, curl) that send no Origin.
	sameOrigin := RequireSameOrigin(d.CORSAllowedOrigins)
	api.POST("/auth/firebase", sameOrigin, ipRateLimit(120.0/60, 300), h.FirebaseAuth)
	api.POST("/auth/refresh", sameOrigin, cookieOrIPRateLimit(RefreshCookieName, 30.0/60, 60), h.AuthRefresh)
	api.POST("/auth/logout", sameOrigin, h.AuthLogout)
	api.POST("/auth/revoke-sessions", auth.RequireAuthoritativeAuth(), auth.RequireOwnerOrManagerOrTenant(d.JWTSecret, d.AuthUserRepo), h.RevokeSessions)
	api.GET("/auth/sessions", auth.RequireAuthoritativeAuth(), auth.RequireOwnerOrManagerOrTenant(d.JWTSecret, d.AuthUserRepo), h.GetActiveSessions)
	api.DELETE("/auth/sessions/:id", auth.RequireAuthoritativeAuth(), auth.RequireOwnerOrManagerOrTenant(d.JWTSecret, d.AuthUserRepo), h.DeleteSession)
	api.GET("/auth/identities", auth.RequireAuthoritativeAuth(), auth.RequireOwnerOrManagerOrTenant(d.JWTSecret, d.AuthUserRepo), h.ListAuthIdentities)
	api.POST("/auth/identities/link", auth.RequireAuthoritativeAuth(), auth.RequireOwnerOrManagerOrTenant(d.JWTSecret, d.AuthUserRepo), h.LinkAuthIdentity)
	api.POST("/auth/identities/merge", auth.RequireAuthoritativeAuth(), auth.RequireOwnerOrManagerOrTenant(d.JWTSecret, d.AuthUserRepo), h.MergeAuthIdentity)

	pending := api.Group("/join", auth.RequirePendingJoin(d.JWTSecret, d.AuthUserRepo))
	{
		pending.GET("/me", h.JoinMe)
		pending.POST("", h.JoinProfile)
		pending.POST("/waitlist", h.JoinWaitlist)
		pending.POST("/waitlist/accept", h.JoinWaitlistAccept)
	}

	owner := api.Group("/owner", auth.RequireOwner(d.JWTSecret, d.AuthUserRepo), h.ResolveOwnerPropertyScope())
	ownerAuth := api.Group("/owner", auth.RequireAuthoritativeAuth(), auth.RequireOwner(d.JWTSecret, d.AuthUserRepo), h.ResolveOwnerPropertyScope())
	registerOwnerRoutes(owner, ownerAuth, h)

	manager := api.Group("/manager", auth.RequireManagerOrOwner(d.JWTSecret, d.AuthUserRepo), h.ResolveOwnerPropertyScope())
	managerAuth := api.Group("/manager", auth.RequireAuthoritativeAuth(), auth.RequireManagerOrOwner(d.JWTSecret, d.AuthUserRepo), h.ResolveOwnerPropertyScope())
	registerManagerRoutes(manager, managerAuth, d, h)

	tenant := api.Group("/tenant", auth.RequireTenant(d.JWTSecret, d.AuthTenantRepo, d.AuthUserRepo))
	tenantAuth := api.Group("/tenant", auth.RequireAuthoritativeAuth(), auth.RequireTenant(d.JWTSecret, d.AuthTenantRepo, d.AuthUserRepo))
	registerTenantRoutes(tenant, tenantAuth, h)

	platform := api.Group("/platform", auth.RequireAuthoritativeAuth(), auth.RequirePlatformOps(d.JWTSecret, d.AuthUserRepo))
	registerPlatformRoutes(platform, h)

	api.GET("/search", auth.RequireOwnerOrManagerOrTenant(d.JWTSecret, d.AuthUserRepo), userOrIPRateLimit(120.0/60, 20), h.Search)
	api.GET("/locales", h.ListLocales)

	prefs := api.Group("/me/preferences", auth.RequireOwnerOrManagerOrTenant(d.JWTSecret, d.AuthUserRepo))
	{
		prefs.GET("", h.GetPreferences)
		prefs.PATCH("", h.PatchPreferences)
	}
	api.GET("/me/onboarding", auth.RequireTenant(d.JWTSecret, d.AuthTenantRepo, d.AuthUserRepo), h.MeOnboarding)

	notifs := api.Group("/notifications", auth.RequireOwnerOrManagerOrTenant(d.JWTSecret, d.AuthUserRepo))
	{
		notifs.GET("", h.ListNotifications)
		notifs.PATCH("/:id/read", h.MarkNotificationRead)
		notifs.PATCH("/read-all", h.MarkAllNotificationsRead)
	}

}

func registerPlatformRoutes(platform *gin.RouterGroup, h *Handlers) {
	payouts := platform.Group("/payouts")
	{
		payouts.GET("/exceptions", h.PlatformListPayoutExceptions)
		payouts.GET("/batches", h.PlatformListPayoutBatches)
		payouts.GET("/batches/:id", h.PlatformGetPayoutBatch)
		payouts.POST("/items/:id/reconcile", h.PlatformReconcilePayoutItem)
		payouts.POST("/items/:id/reconcile-case", h.PlatformCreatePayoutReconciliationCase)
		payouts.GET("/reconcile-cases", h.PlatformListPayoutReconciliationCases)
		payouts.GET("/reconcile-cases/:id", h.PlatformGetPayoutReconciliationCase)
		payouts.POST("/reconcile-cases/:id/approve", h.PlatformApprovePayoutReconciliationCase)
		payouts.POST("/reconcile-cases/:id/reject", h.PlatformRejectPayoutReconciliationCase)
	}
}
