package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/magiclink"
)

func TestPaymentPage_Localization(t *testing.T) {
	gin.SetMode(gin.TestMode)

	mockView := &magiclink.DueView{
		TokenID:      uuid.New(),
		Due:          domain.Due{DueCode: "DUE-1001", Status: domain.DueStatusPending},
		AmountPaise:  1500000,
		OwnerName:    "Ramesh Kumar",
		PropertyName: "Sunrise PG",
		ExpiresAt:    time.Now().Add(24 * time.Hour),
	}

	h := &Handlers{
		Deps: Deps{
			MagicLink: &mockMagicLinkService{view: mockView},
		},
	}

	r := gin.New()
	r.GET("/p/:token", h.PaymentPage)

	t.Run("default English", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/p/token123", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, `<html lang="en">`) {
			t.Errorf("expected lang='en', got: %s", body)
		}
		if !strings.Contains(body, "Pay rent") {
			t.Errorf("expected English text 'Pay rent', got: %s", body)
		}
		if !strings.Contains(body, "Log in to Pay Due") {
			t.Errorf("expected English button 'Log in to Pay Due', got: %s", body)
		}
	})

	t.Run("query param Telugu", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/p/token123?lang=te", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, `<html lang="te">`) {
			t.Errorf("expected lang='te', got: %s", body)
		}
		if !strings.Contains(body, "అద్దె చెల్లించండి") {
			t.Errorf("expected Telugu title, got: %s", body)
		}
		if !strings.Contains(body, "బాకీ చెల్లించడానికి లాగిన్ అవ్వండి") {
			t.Errorf("expected Telugu button, got: %s", body)
		}
	})

	t.Run("query param Tamil", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/p/token123?lang=ta", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, `<html lang="ta">`) {
			t.Errorf("expected lang='ta', got: %s", body)
		}
		if !strings.Contains(body, "வாடகை செலுத்தவும்") {
			t.Errorf("expected Tamil title, got: %s", body)
		}
	})

	t.Run("query param Kannada", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/p/token123?lang=kn", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, `<html lang="kn">`) {
			t.Errorf("expected lang='kn', got: %s", body)
		}
		if !strings.Contains(body, "ಬಾಡಿಗೆ ಪಾವತಿಸಿ") {
			t.Errorf("expected Kannada title, got: %s", body)
		}
	})

	t.Run("Accept-Language header fallback", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/p/token123", nil)
		req.Header.Set("Accept-Language", "te-IN,te;q=0.9,en;q=0.8")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, `<html lang="te">`) {
			t.Errorf("expected lang='te' via Accept-Language, got: %s", body)
		}
	})

	t.Run("JSON negotiation returns locale", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/p/token123?lang=te", nil)
		req.Header.Set("Accept", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
		var out struct {
			Locale string `json:"locale"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("invalid json: %v", err)
		}
		if out.Locale != "te-IN" {
			t.Errorf("expected locale te-IN, got %s", out.Locale)
		}
	})
}
