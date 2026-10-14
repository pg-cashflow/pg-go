package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

func registerPublicRoutes(r *gin.Engine, d Deps, h *Handlers, healthHandler gin.HandlerFunc) {
	r.GET("/healthz", healthHandler)
	r.GET("/metrics", metricsAuth(d.JWTSecret, d.MetricsSecret), h.Metrics)
	r.GET("/p/:token", h.PaymentPage)
	r.POST("/p/:token/push/subscribe", h.PaymentPushSubscribe)
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)
	r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
	r.POST("/webhooks/cashfree/settlements", h.CashfreeSettlementWebhook)
}

func healthHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}
