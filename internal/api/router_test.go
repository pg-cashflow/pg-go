package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRedactingLogFormatter(t *testing.T) {
	buf := new(bytes.Buffer)
	r := gin.New()
	r.Use(gin.LoggerWithConfig(gin.LoggerConfig{
		Output:    buf,
		Formatter: redactingLogFormatter,
	}))

	r.GET("/p/:token", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})

	secretToken := "secret-magic-token-xyz-12345"
	req := httptest.NewRequest(http.MethodGet, "/p/"+secretToken, nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	logOutput := buf.String()
	if strings.Contains(logOutput, secretToken) {
		t.Fatalf("security violation: log output leaked magic link token %q: %s", secretToken, logOutput)
	}

	if !strings.Contains(logOutput, "/p/[REDACTED]") {
		t.Fatalf("expected log output to contain /p/[REDACTED], got: %s", logOutput)
	}
}
