package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/magiclink"
)

type mockPushService struct {
	subscribedTenantID uuid.UUID
	endpoint           string
	p256dh             string
	auth               string
	err                error
}

func (m *mockPushService) Subscribe(ctx context.Context, tenantID uuid.UUID, endpoint, p256dh, auth string) error {
	m.subscribedTenantID = tenantID
	m.endpoint = endpoint
	m.p256dh = p256dh
	m.auth = auth
	return m.err
}

type mockMagicLinkService struct {
	view *magiclink.DueView
	err  error
}

func (m *mockMagicLinkService) CreatePaymentToken(ctx context.Context, dueID uuid.UUID) (string, error) {
	return "", nil
}

func (m *mockMagicLinkService) ResolveToken(ctx context.Context, token string) (*magiclink.DueView, error) {
	return m.view, m.err
}

func TestTenantPushSubscribe(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tenantID := uuid.New()
	ten := &domain.Tenant{ID: tenantID, PropertyID: uuid.New()}
	mockPush := &mockPushService{}
	h := &Handlers{Deps: Deps{Push: mockPush}}

	r := gin.New()
	r.POST("/tenant/push/subscribe", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, ten)
		h.TenantPushSubscribe(c)
	})

	body := map[string]any{
		"endpoint": "https://fcm.googleapis.com/fcm/send/test-endpoint",
		"keys": map[string]string{
			"p256dh": "BNcRdreALRFXTkOOUHK18WK25ypqDPY",
			"auth":   "tBHItJAhVoNL",
		},
	}
	raw, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/tenant/push/subscribe", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}
	if mockPush.subscribedTenantID != tenantID {
		t.Fatalf("expected tenantID %v, got %v", tenantID, mockPush.subscribedTenantID)
	}
	if mockPush.endpoint != "https://fcm.googleapis.com/fcm/send/test-endpoint" {
		t.Fatalf("expected endpoint saved, got %s", mockPush.endpoint)
	}
}

func TestPaymentPushSubscribe(t *testing.T) {
	gin.SetMode(gin.TestMode)

	tenantID := uuid.New()
	mockPush := &mockPushService{}
	mockML := &mockMagicLinkService{
		view: &magiclink.DueView{
			Due: domain.Due{
				ID:       uuid.New(),
				TenantID: tenantID,
			},
			ExpiresAt: time.Now().Add(24 * time.Hour),
		},
	}
	h := &Handlers{Deps: Deps{Push: mockPush, MagicLink: mockML}}

	r := gin.New()
	r.POST("/p/:token/push/subscribe", h.PaymentPushSubscribe)

	body := map[string]any{
		"endpoint": "https://web.push.apple.com/test-endpoint",
		"keys": map[string]string{
			"p256dh": "appleP256Key==",
			"auth":   "appleAuthKey==",
		},
	}
	raw, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPost, "/p/valid-token-123/push/subscribe", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}
	if mockPush.subscribedTenantID != tenantID {
		t.Fatalf("expected tenantID %v, got %v", tenantID, mockPush.subscribedTenantID)
	}
	if mockPush.endpoint != "https://web.push.apple.com/test-endpoint" {
		t.Fatalf("expected apple push endpoint, got %s", mockPush.endpoint)
	}
}
