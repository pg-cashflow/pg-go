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
	t.Setenv("METRICS_ALLOW_OPEN", "true")
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
	t.Setenv("METRICS_ALLOW_OPEN", "true")
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

func TestMetricsEndpoint_AuthClosedByDefault(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handlers{}

	r := gin.New()
	r.GET("/metrics", h.Metrics)

	// 1. By default with no token and no METRICS_ALLOW_OPEN, must fail closed with 401
	t.Setenv("METRICS_TOKEN", "")
	t.Setenv("METRICS_ALLOW_OPEN", "")

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401 Unauthorized by default without token, got %d", w.Code)
	}

	// 2. With token configured, valid token succeeds
	t.Setenv("METRICS_TOKEN", "secret-test-token")
	reqAuth := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	reqAuth.Header.Set("Authorization", "Bearer secret-test-token")
	wAuth := httptest.NewRecorder()
	r.ServeHTTP(wAuth, reqAuth)

	if wAuth.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK with valid bearer token, got %d", wAuth.Code)
	}

	// 3. With token configured, invalid token fails with 401
	reqBadAuth := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	reqBadAuth.Header.Set("Authorization", "Bearer wrong-token")
	wBadAuth := httptest.NewRecorder()
	r.ServeHTTP(wBadAuth, reqBadAuth)

	if wBadAuth.Code != http.StatusUnauthorized {
		t.Fatalf("expected status 401 Unauthorized with invalid bearer token, got %d", wBadAuth.Code)
	}

	// 4. With METRICS_ALLOW_OPEN=true and no token, succeeds without auth
	t.Setenv("METRICS_TOKEN", "")
	t.Setenv("METRICS_ALLOW_OPEN", "true")
	reqOpen := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	wOpen := httptest.NewRecorder()
	r.ServeHTTP(wOpen, reqOpen)

	if wOpen.Code != http.StatusOK {
		t.Fatalf("expected status 200 OK when METRICS_ALLOW_OPEN=true, got %d", wOpen.Code)
	}
}
