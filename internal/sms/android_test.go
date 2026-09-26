package sms

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAndroidGateway_PrimarySuccess(t *testing.T) {
	primaryCalled := 0
	fallbackCalled := 0
	alertCalled := false

	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalled++
		if r.Header.Get("X-API-Key") != "primary-key" {
			t.Errorf("expected X-API-Key 'primary-key', got %q", r.Header.Get("X-API-Key"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer primaryServer.Close()

	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalled++
		w.WriteHeader(http.StatusOK)
	}))
	defer fallbackServer.Close()

	gw := &AndroidGateway{
		PrimaryURL:     primaryServer.URL,
		PrimaryAPIKey:  "primary-key",
		FallbackURL:    fallbackServer.URL,
		FallbackAPIKey: "fallback-key",
		Alert: func(ctx context.Context, phone, message string, primaryErr, fallbackErr error) {
			alertCalled = true
		},
	}

	err := gw.Send(context.Background(), "9876543210", "Test message")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if primaryCalled != 1 {
		t.Fatalf("expected 1 primary call, got %d", primaryCalled)
	}
	if fallbackCalled != 0 {
		t.Fatalf("expected 0 fallback calls, got %d", fallbackCalled)
	}
	if alertCalled {
		t.Fatal("expected alert not to be called")
	}
}

func TestAndroidGateway_FallbackSuccessOnPrimaryFailure(t *testing.T) {
	primaryCalled := 0
	fallbackCalled := 0
	alertCalled := false

	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalled++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primaryServer.Close()

	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalled++
		if r.Header.Get("X-API-Key") != "fallback-key" {
			t.Errorf("expected X-API-Key 'fallback-key', got %q", r.Header.Get("X-API-Key"))
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer fallbackServer.Close()

	gw := &AndroidGateway{
		PrimaryURL:     primaryServer.URL,
		PrimaryAPIKey:  "primary-key",
		FallbackURL:    fallbackServer.URL,
		FallbackAPIKey: "fallback-key",
		Alert: func(ctx context.Context, phone, message string, primaryErr, fallbackErr error) {
			alertCalled = true
		},
	}

	err := gw.Send(context.Background(), "9876543210", "Test message")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if primaryCalled != 1 {
		t.Fatalf("expected 1 primary call, got %d", primaryCalled)
	}
	if fallbackCalled != 1 {
		t.Fatalf("expected 1 fallback call, got %d", fallbackCalled)
	}
	if alertCalled {
		t.Fatal("expected alert not to be called on fallback success")
	}
}

func TestAndroidGateway_BothFailAlerts(t *testing.T) {
	primaryCalled := 0
	fallbackCalled := 0
	var capturedPrimaryErr error
	var capturedFallbackErr error

	primaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalled++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer primaryServer.Close()

	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackCalled++
		w.WriteHeader(http.StatusGatewayTimeout)
	}))
	defer fallbackServer.Close()

	gw := &AndroidGateway{
		PrimaryURL:     primaryServer.URL,
		PrimaryAPIKey:  "primary-key",
		FallbackURL:    fallbackServer.URL,
		FallbackAPIKey: "fallback-key",
		Alert: func(ctx context.Context, phone, message string, pErr, fErr error) {
			capturedPrimaryErr = pErr
			capturedFallbackErr = fErr
		},
	}

	err := gw.Send(context.Background(), "9876543210", "Test message")
	if err == nil {
		t.Fatal("expected error when both fail, got nil")
	}
	if primaryCalled != 1 || fallbackCalled != 1 {
		t.Fatalf("expected 1 call each, got primary=%d, fallback=%d", primaryCalled, fallbackCalled)
	}
	if capturedPrimaryErr == nil || capturedFallbackErr == nil {
		t.Fatal("expected Alert to receive both errors")
	}
}

func TestAndroidGateway_EmptyURL(t *testing.T) {
	gw := &AndroidGateway{}
	err := gw.Send(context.Background(), "9876543210", "Test")
	if err == nil {
		t.Fatal("expected error on empty URLs")
	}
}
