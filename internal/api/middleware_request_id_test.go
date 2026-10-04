package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRequestIDMiddleware_GeneratedWhenMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestIDMiddleware())

	var capturedCtxID string
	r.GET("/test", func(c *gin.Context) {
		capturedCtxID = RequestIDFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	respID := w.Header().Get(RequestIDHeader)
	if respID == "" {
		t.Fatalf("expected non-empty %s header in response", RequestIDHeader)
	}
	if capturedCtxID != respID {
		t.Errorf("expected context ID %s to match response header %s", capturedCtxID, respID)
	}
}

func TestRequestIDMiddleware_PropagatesExisting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(RequestIDMiddleware())

	existingID := "req-trace-xyz-12345"
	var capturedCtxID string
	r.GET("/test", func(c *gin.Context) {
		capturedCtxID = RequestIDFromContext(c.Request.Context())
		c.Status(http.StatusOK)
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set(RequestIDHeader, existingID)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	respID := w.Header().Get(RequestIDHeader)
	if respID != existingID {
		t.Errorf("expected %s, got %s", existingID, respID)
	}
	if capturedCtxID != existingID {
		t.Errorf("expected context %s, got %s", existingID, capturedCtxID)
	}
}
