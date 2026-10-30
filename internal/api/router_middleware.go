package api

import (
	"crypto/subtle"
	"fmt"
	"net"
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
	return fmt.Sprintf("[GIN] %s | %3d | %13v | %-7s %s\n",
		p.TimeStamp.Format("2006/01/02 - 15:04:05"),
		p.StatusCode,
		p.Latency,
		p.Method,
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

// SecurityHeaders adds baseline HTTP security headers (nosniff, frame denial, referrer policy, CSP, HSTS).
func SecurityHeaders(appEnv string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Permissions-Policy", "camera=(self), microphone=(), geolocation=()")
		c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self' https://sdk.cashfree.com https://apis.google.com https://www.google.com/recaptcha/ https://www.gstatic.com/recaptcha/; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; img-src 'self' data: blob: https:; font-src 'self' data: https://fonts.gstatic.com; connect-src 'self' https://sandbox.cashfree.com https://api.cashfree.com https://identitytoolkit.googleapis.com https://securetoken.googleapis.com https://firebaseappcheck.googleapis.com https://*.googleapis.com; frame-src 'self' https://sdk.cashfree.com https://api.cashfree.com https://sandbox.cashfree.com https://payments.cashfree.com https://payments-test.cashfree.com https://www.google.com/recaptcha/ https://recaptcha.google.com https://*.firebaseapp.com; object-src 'none'; base-uri 'self'; frame-ancestors 'none';")

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
	if strings.EqualFold(d.AppEnv, "production") {
		return false
	}
	return true
}

// CloudflareRealIP reads the CF-Connecting-IP header only when the peer matches
// one of the trusted proxies (or loopback). This prevents direct-to-port-8080 callers
// from forging client IP addresses and bypassing rate limits or audit logs.
func CloudflareRealIP(trustedProxies []string) gin.HandlerFunc {
	return func(c *gin.Context) {
		cfIP := strings.TrimSpace(c.GetHeader("CF-Connecting-IP"))
		if cfIP != "" && isTrustedProxyPeer(c.Request.RemoteAddr, trustedProxies) {
			// Preserve the existing port so that net.SplitHostPort works on RemoteAddr.
			c.Request.RemoteAddr = cfIP + ":0"
		}
		c.Next()
	}
}

func isTrustedProxyPeer(remoteAddr string, trusted []string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(strings.TrimSpace(host))
	if ip == nil {
		return false
	}
	if len(trusted) == 0 {
		return ip.IsLoopback()
	}
	for _, t := range trusted {
		t = strings.TrimSpace(t)
		if strings.Contains(t, "/") {
			_, ipNet, err := net.ParseCIDR(t)
			if err == nil && ipNet.Contains(ip) {
				return true
			}
		} else {
			if parsed := net.ParseIP(t); parsed != nil && parsed.Equal(ip) {
				return true
			}
		}
	}
	return false
}
