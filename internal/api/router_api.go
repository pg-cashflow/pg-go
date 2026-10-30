package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/auth"
)

func registerAPIRoutes(r *gin.Engine, d Deps, h *Handlers, health gin.HandlerFunc) {
	api := r.Group("/api")
	api.GET("/healthz", health)
	api.GET("/metrics", metricsAuth(d.JWTSecret, d.MetricsSecret), h.Metrics)
	api.GET("/push/vapid-public-key", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"public_key": d.VAPIDPublicKey})
	})
	api.GET("/join/invite/:code", ipRateLimit(60.0/60, 100), h.LookupInvite)
	api.GET("/owner/calendar.ics", h.OwnerCalendarICS)

	api.POST("/auth/otp/request", ipRateLimit(3.0/60, 5), h.OTPRequest)
	api.POST("/auth/otp/verify", ipRateLimit(10.0/60, 20), h.OTPVerify)
	api.POST("/auth/firebase", ipRateLimit(60.0/60, 100), h.FirebaseAuth)
	api.POST("/auth/refresh", cookieOrIPRateLimit(RefreshCookieName, 30.0/60, 60), h.AuthRefresh)
	api.POST("/auth/logout", h.AuthLogout)
	api.POST("/auth/revoke-sessions", auth.RequireOwnerOrManagerOrTenant(d.JWTSecret, d.AuthUserRepo), h.RevokeSessions)
	api.GET("/auth/sessions", auth.RequireOwnerOrManagerOrTenant(d.JWTSecret, d.AuthUserRepo), h.GetActiveSessions)
	api.DELETE("/auth/sessions/:id", auth.RequireOwnerOrManagerOrTenant(d.JWTSecret, d.AuthUserRepo), h.DeleteSession)

	pending := api.Group("/join", auth.RequirePendingJoin(d.JWTSecret, d.AuthUserRepo))
	{
		pending.GET("/me", h.JoinMe)
		pending.POST("", h.JoinProfile)
		pending.POST("/waitlist", h.JoinWaitlist)
		pending.POST("/waitlist/accept", h.JoinWaitlistAccept)
	}

	owner := api.Group("/owner", auth.RequireOwner(d.JWTSecret, d.AuthUserRepo), h.ResolveOwnerPropertyScope())
	registerOwnerRoutes(owner, h)

	manager := api.Group("/manager", auth.RequireManagerOrOwner(d.JWTSecret, d.AuthUserRepo), h.ResolveOwnerPropertyScope())
	registerManagerRoutes(manager, d, h)

	tenant := api.Group("/tenant", auth.RequireTenant(d.JWTSecret, d.AuthTenantRepo, d.AuthUserRepo))
	registerTenantRoutes(tenant, h)

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
