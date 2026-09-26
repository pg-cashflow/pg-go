package push

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

type mockPushRepo struct {
	upsertCalled   bool
	deletedEndpoint string
	subs           []Subscription
}

func (m *mockPushRepo) Upsert(ctx context.Context, s *Subscription) error {
	m.upsertCalled = true
	return nil
}

func (m *mockPushRepo) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]Subscription, error) {
	return m.subs, nil
}

func (m *mockPushRepo) DeleteByEndpoint(ctx context.Context, endpoint string) error {
	m.deletedEndpoint = endpoint
	return nil
}

func (m *mockPushRepo) DeleteByTenant(ctx context.Context, tenantID uuid.UUID) error {
	return nil
}

func TestPushService_SubscribeValidation(t *testing.T) {
	repo := &mockPushRepo{}
	svc := NewService(repo, Config{}, nil)
	tenantID := uuid.New()

	tests := []struct {
		name     string
		endpoint string
		p256dh   string
		auth     string
		wantErr  bool
	}{
		{"empty endpoint", "", "p256dh-key", "auth-token", true},
		{"empty p256dh", "https://push.example.com/sub", "", "auth-token", true},
		{"empty auth", "https://push.example.com/sub", "p256dh-key", "", true},
		{"valid subscription", "https://push.example.com/sub", "p256dh-key", "auth-token", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := svc.Subscribe(context.Background(), tenantID, tt.endpoint, tt.p256dh, tt.auth)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Subscribe() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestPushService_Unsubscribe(t *testing.T) {
	repo := &mockPushRepo{}
	svc := NewService(repo, Config{}, nil)

	endpoint := "https://push.example.com/sub123"
	if err := svc.Unsubscribe(context.Background(), endpoint); err != nil {
		t.Fatalf("Unsubscribe failed: %v", err)
	}
	if repo.deletedEndpoint != endpoint {
		t.Fatalf("expected deleted endpoint %q, got %q", endpoint, repo.deletedEndpoint)
	}
}

func TestPushService_EmptyVAPIDSkipsSend(t *testing.T) {
	repo := &mockPushRepo{}
	svc := NewService(repo, Config{VAPIDPublicKey: "", VAPIDPrivateKey: ""}, nil)

	// When VAPID keys are empty, Send must log and return nil without attempting network delivery
	err := svc.Send(context.Background(), uuid.New(), []byte(`{"title":"Test"}`))
	if err != nil {
		t.Fatalf("expected nil error on empty VAPID, got: %v", err)
	}
}
