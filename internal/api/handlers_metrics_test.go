package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestMetricsEndpoint_JSON(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handlers{}

	r := gin.New()
	r.GET("/metrics", h.Metrics)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d", w.Code)
	}

	var resp MetricsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal metrics response: %v", err)
	}

	if resp.Status != "ok" {
		t.Errorf("expected status 'ok', got '%s'", resp.Status)
	}
	if resp.Runtime.NumGoroutine <= 0 {
		t.Errorf("expected positive num_goroutines, got %d", resp.Runtime.NumGoroutine)
	}
}

func TestMetricsEndpoint_Prometheus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handlers{}

	r := gin.New()
	r.GET("/metrics", h.Metrics)

	req := httptest.NewRequest(http.MethodGet, "/metrics?format=prometheus", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "go_goroutines") {
		t.Errorf("expected prometheus output to contain 'go_goroutines', got:\n%s", body)
	}
	if !strings.Contains(body, "pg_pool_total_connections") {
		t.Errorf("expected prometheus output to contain 'pg_pool_total_connections', got:\n%s", body)
	}
	if !strings.Contains(body, "pg_query_duration_p95_ms") {
		t.Errorf("expected prometheus output to contain 'pg_query_duration_p95_ms', got:\n%s", body)
	}
}

func TestMetricsEndpoint_ProductionAuthRequired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handlers{}

	r := gin.New()
	r.GET("/metrics", h.Metrics)

	// In production with no token configured, must fail closed with 401
	t.Setenv("APP_ENV", "production")
	t.Setenv("METRICS_TOKEN", "")

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401 Unauthorized in production without token, got %d", w.Code)
	}

	// In production with token configured, valid token succeeds
	t.Setenv("METRICS_TOKEN", "secret-test-token")
	reqAuth := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	reqAuth.Header.Set("Authorization", "Bearer secret-test-token")
	wAuth := httptest.NewRecorder()
	r.ServeHTTP(wAuth, reqAuth)

	if wAuth.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK with valid bearer token, got %d", wAuth.Code)
	}
}
