package sms

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// AlertFunc is invoked when both primary and fallback SMS sends fail.
type AlertFunc func(ctx context.Context, phone, message string, primaryErr, fallbackErr error)

// AndroidGateway POSTs JSON {phone, message} to an Android SMS relay.
// Tries PrimaryURL first, then FallbackURL; on both failures calls Alert.
type AndroidGateway struct {
	PrimaryURL      string
	PrimaryAPIKey   string
	FallbackURL     string
	FallbackAPIKey  string
	HTTPClient      *http.Client
	Alert           AlertFunc
}

func (g *AndroidGateway) client() *http.Client {
	if g.HTTPClient != nil {
		return g.HTTPClient
	}
	return &http.Client{Timeout: 15 * time.Second}
}

// Send tries the primary gateway, then the fallback. If both fail, Alert is called (if set).
func (g *AndroidGateway) Send(ctx context.Context, phone, message string) error {
	primaryErr := g.post(ctx, g.PrimaryURL, g.PrimaryAPIKey, phone, message)
	if primaryErr == nil {
		return nil
	}

	fallbackErr := g.post(ctx, g.FallbackURL, g.FallbackAPIKey, phone, message)
	if fallbackErr == nil {
		return nil
	}

	if g.Alert != nil {
		g.Alert(ctx, phone, message, primaryErr, fallbackErr)
	}
	return fmt.Errorf("sms primary: %v; fallback: %w", primaryErr, fallbackErr)
}

func (g *AndroidGateway) post(ctx context.Context, url, apiKey, phone, message string) error {
	if url == "" {
		return fmt.Errorf("sms gateway url empty")
	}
	body, err := json.Marshal(map[string]string{
		"phone":   phone,
		"message": message,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := g.client().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sms gateway status %d", resp.StatusCode)
	}
	return nil
}
