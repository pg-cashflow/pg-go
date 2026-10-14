package api

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

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

// metricsAuth is the only gate for GET /metrics and GET /api/metrics.
func metricsAuth(jwtSecret, metricsSecret string) gin.HandlerFunc {
	_ = jwtSecret
	return func(c *gin.Context) {
		allowOpen := strings.EqualFold(os.Getenv("METRICS_ALLOW_OPEN"), "true")
		if metricsSecret != "" {
			tok := c.GetHeader("X-Metrics-Token")
			if subtle.ConstantTimeCompare([]byte(tok), []byte(metricsSecret)) == 1 {
				c.Next()
				return
			}
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "metrics endpoint requires X-Metrics-Token"})
			return
		}
		if allowOpen {
			c.Next()
			return
		}
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "metrics endpoint requires authentication"})
	}
}

// SecurityHeaders adds baseline HTTP security headers (nosniff, frame denial, referrer policy, CSP Report-Only, HSTS).
func SecurityHeaders(appEnv string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		c.Header("Content-Security-Policy-Report-Only", "default-src 'self'; script-src 'self' 'unsafe-inline' https://sdk.cashfree.com; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob: https:; font-src 'self' data:; connect-src 'self' https://sandbox.cashfree.com https://api.cashfree.com; frame-src 'self' https://sdk.cashfree.com https://api.cashfree.com;")

		if strings.EqualFold(appEnv, "production") {
			c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		c.Next()
	}
}

// MaxBodyBytes limits the incoming request body size to prevent memory exhaustion / DoS attacks.
func MaxBodyBytes(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		}
		c.Next()
	}
}

func legacyAppShellEnabled(d Deps) bool {
	if !strings.EqualFold(d.AppEnv, "production") {
		return true
	}
	return strings.TrimSpace(d.FrontendURL) == ""
}
