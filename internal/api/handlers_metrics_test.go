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
	const secret = "metrics-secret-16"

	mount := func(metricsSecret string) *gin.Engine {
		r := gin.New()
		r.GET("/metrics", metricsAuth("jwt-secret-not-used-for-metrics", metricsSecret), h.Metrics)
		return r
	}

	t.Setenv("METRICS_ALLOW_OPEN", "")
	t.Setenv("METRICS_TOKEN", "ignored-legacy-token")

	w := httptest.NewRecorder()
	mount("").ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 without secret and without METRICS_ALLOW_OPEN, got %d", w.Code)
	}

	wTok := httptest.NewRecorder()
	reqTok := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	reqTok.Header.Set("X-Metrics-Token", secret)
	mount(secret).ServeHTTP(wTok, reqTok)
	if wTok.Code != http.StatusOK {
		t.Fatalf("expected 200 with X-Metrics-Token, got %d", wTok.Code)
	}

	wJWT := httptest.NewRecorder()
	reqJWT := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	reqJWT.Header.Set("Authorization", "Bearer owner-jwt")
	mount("").ServeHTTP(wJWT, reqJWT)
	if wJWT.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for JWT without metrics secret, got %d", wJWT.Code)
	}

	t.Setenv("METRICS_ALLOW_OPEN", "true")
	wOpen := httptest.NewRecorder()
	mount("").ServeHTTP(wOpen, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if wOpen.Code != http.StatusOK {
		t.Fatalf("expected 200 when METRICS_ALLOW_OPEN=true, got %d", wOpen.Code)
	}
}
