package push

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"

	"github.com/SherClockHolmes/webpush-go"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// ErrInvalidPushEndpoint indicates that the push endpoint URL failed security validation.
var ErrInvalidPushEndpoint = errors.New("push: invalid or disallowed endpoint URL")

func validatePushEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("%w: parse error: %v", ErrInvalidPushEndpoint, err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%w: scheme must be https", ErrInvalidPushEndpoint)
	}
	if u.User != nil {
		return fmt.Errorf("%w: userinfo not allowed", ErrInvalidPushEndpoint)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: missing host", ErrInvalidPushEndpoint)
	}
	port := u.Port()
	if port != "" && port != "443" {
		return fmt.Errorf("%w: non-standard port not allowed", ErrInvalidPushEndpoint)
	}

	if !isAllowedPushHost(strings.ToLower(host)) {
		return fmt.Errorf("%w: untrusted host %q", ErrInvalidPushEndpoint, host)
	}
	return nil
}

func isAllowedPushHost(host string) bool {
	allowedSuffixes := []string{
		"fcm.googleapis.com",
		"push.apple.com",
		"notify.windows.com",
		"push.services.mozilla.com",
	}
	for _, allowed := range allowedSuffixes {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return true
		}
	}
	return false
}

// Service manages Web Push subscriptions and delivery.
type Service struct {
	repo            Repository
	vapidPublicKey  string
	vapidPrivateKey string
	subject         string
	log             *slog.Logger
}

// Config holds VAPID credentials from env.
type Config struct {
	VAPIDPublicKey  string
	VAPIDPrivateKey string
	Subject         string // e.g. mailto:owner@example.com
}

func NewService(repo Repository, cfg Config, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		repo:            repo,
		vapidPublicKey:  cfg.VAPIDPublicKey,
		vapidPrivateKey: cfg.VAPIDPrivateKey,
		subject:         cfg.Subject,
		log:             log,
	}
}

// Subscribe upserts a browser push subscription for the tenant.
func (s *Service) Subscribe(ctx context.Context, tenantID uuid.UUID, endpoint, p256dh, auth string) error {
	if endpoint == "" || p256dh == "" || auth == "" {
		return fmt.Errorf("push: endpoint, p256dh, and auth are required")
	}
	if err := validatePushEndpoint(endpoint); err != nil {
		return err
	}
	sub := &Subscription{
		TenantID: tenantID,
		Endpoint: endpoint,
		P256dh:   p256dh,
		Auth:     auth,
	}
	return s.repo.Upsert(ctx, sub)
}

// Unsubscribe removes a subscription by endpoint.
func (s *Service) Unsubscribe(ctx context.Context, endpoint string) error {
	return s.repo.DeleteByEndpoint(ctx, endpoint)
}

// Send delivers payload to all subscriptions for tenantID.
// If VAPID keys are empty, logs and returns nil (dev stub).
func (s *Service) Send(ctx context.Context, tenantID uuid.UUID, payload []byte) error {
	if s.vapidPublicKey == "" || s.vapidPrivateKey == "" {
		s.log.Info("push: VAPID keys empty; skipping send", "tenant_id", tenantID, "payload_len", len(payload))
		return nil
	}
	subs, err := s.repo.ListByTenant(ctx, tenantID)
	if err != nil {
		return err
	}
	var firstErr error
	for _, sub := range subs {
		wpSub := &webpush.Subscription{
			Endpoint: sub.Endpoint,
			Keys: webpush.Keys{
				P256dh: sub.P256dh,
				Auth:   sub.Auth,
			},
		}
		resp, err := webpush.SendNotificationWithContext(ctx, payload, wpSub, &webpush.Options{
			Subscriber:      s.subject,
			VAPIDPublicKey:  s.vapidPublicKey,
			VAPIDPrivateKey: s.vapidPrivateKey,
			TTL:             60,
		})
		if err != nil {
			s.log.Warn("push: send failed", "endpoint", sub.Endpoint, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if resp != nil {
			_ = resp.Body.Close()
			// Gone / Not Found → drop dead endpoint
			if resp.StatusCode == 404 || resp.StatusCode == 410 {
				_ = s.repo.DeleteByEndpoint(ctx, sub.Endpoint)
			}
		}
	}
	return firstErr
}

// RepoAdapter adapts postgres.PushRepo to Repository.
type RepoAdapter struct {
	Inner *postgres.PushRepo
}

func (a RepoAdapter) Upsert(ctx context.Context, s *Subscription) error {
	ps := &postgres.PushSubscription{
		ID:        s.ID,
		TenantID:  s.TenantID,
		Endpoint:  s.Endpoint,
		P256dh:    s.P256dh,
		Auth:      s.Auth,
		CreatedAt: s.CreatedAt,
	}
	if err := a.Inner.Upsert(ctx, ps); err != nil {
		return err
	}
	s.ID = ps.ID
	s.CreatedAt = ps.CreatedAt
	return nil
}

func (a RepoAdapter) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]Subscription, error) {
	rows, err := a.Inner.ListByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	out := make([]Subscription, 0, len(rows))
	for _, r := range rows {
		out = append(out, Subscription{
			ID:        r.ID,
			TenantID:  r.TenantID,
			Endpoint:  r.Endpoint,
			P256dh:    r.P256dh,
			Auth:      r.Auth,
			CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

func (a RepoAdapter) DeleteByEndpoint(ctx context.Context, endpoint string) error {
	return a.Inner.DeleteByEndpoint(ctx, endpoint)
}

func (a RepoAdapter) DeleteByTenant(ctx context.Context, tenantID uuid.UUID) error {
	return a.Inner.DeleteByTenant(ctx, tenantID)
}
