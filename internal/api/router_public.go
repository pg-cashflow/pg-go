package api

import (
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// WorkerStatus captures the last-known health of a background worker.
type WorkerStatus struct {
	Healthy       bool       `json:"healthy"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	LastError     string     `json:"last_error,omitempty"`
}

// WorkerHealthReporter exposes background worker health separately from HTTP readiness.
type WorkerHealthReporter interface {
	Snapshot() map[string]WorkerStatus
}

func registerPublicRoutes(r *gin.Engine, d Deps, h *Handlers, healthHandler gin.HandlerFunc) {
	r.GET("/healthz", healthHandler)
	r.GET("/healthz/workers", workerHealthHandler(d.WorkerHealth))
	r.GET("/metrics", metricsAuth(d.JWTSecret, d.MetricsSecret), h.Metrics)
	r.GET("/p/:token", h.PaymentPage)
	r.POST("/p/:token/push/subscribe", h.PaymentPushSubscribe)
	r.POST("/webhooks/cashfree", h.CashfreeWebhook)
	r.POST("/webhooks/cashfree/payouts", h.CashfreePayoutWebhook)
	r.POST("/webhooks/cashfree/settlements", h.CashfreeSettlementWebhook)
	r.POST("/webhooks/cashfree/kyc", h.CashfreeKYCWebhook)
}

func healthHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	}
}

func workerHealthHandler(reporter WorkerHealthReporter) gin.HandlerFunc {
	return func(c *gin.Context) {
		if reporter == nil {
			c.JSON(http.StatusOK, gin.H{"status": "ok", "workers": map[string]WorkerStatus{}})
			return
		}
		workers := reporter.Snapshot()
		overall := "ok"
		code := http.StatusOK
		for _, st := range workers {
			if !st.Healthy {
				overall = "degraded"
				code = http.StatusServiceUnavailable
				break
			}
		}
		c.JSON(code, gin.H{"status": overall, "workers": workers})
	}
}
