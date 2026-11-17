package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type fakeDigiLockerKYCSvc struct {
	fakeKYCSvc
}

func (f *fakeDigiLockerKYCSvc) PollDigiLockerStatus(ctx context.Context, tenantID uuid.UUID, actor string) (*domain.KYCVerification, error) {
	return &domain.KYCVerification{
		ID:       uuid.New(),
		TenantID: tenantID,
		Status:   domain.KYCStatusVerified,
		Method:   domain.KYCMethodDigiLocker,
	}, nil
}

func TestTenantKYCDigiLocker_Initiate_OK(t *testing.T) {
	tenantID := uuid.New()
	tenant := &domain.Tenant{ID: tenantID, PropertyID: uuid.New()}

	svc := &fakeKYCSvc{}
	r := kycTestTenantRouterWithTenant(svc, "", tenant)
	reqBody := `{"redirect_url":"https://app.pgflow.in/kyc/callback","user_flow":"signin"}`
	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/digilocker/init", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		RedirectURL    string         `json:"redirect_url"`
		VerificationID string         `json:"verification_id"`
		Verification   map[string]any `json:"verification"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if !strings.HasPrefix(resp.RedirectURL, "https://verification.cashfree.com") {
		t.Fatalf("unexpected redirect_url: %s", resp.RedirectURL)
	}
	if resp.Verification["method"] != "digilocker" || resp.Verification["status"] != "pending" {
		t.Fatalf("unexpected verification view: %v", resp.Verification)
	}
}

func TestTenantKYCDigiLocker_Initiate_MissingRedirect(t *testing.T) {
	r := kycTestTenantRouter(&fakeKYCSvc{}, "")
	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/digilocker/init", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing redirect_url, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTenantKYCDigiLocker_Status_OK(t *testing.T) {
	tenantID := uuid.New()
	tenant := &domain.Tenant{ID: tenantID, PropertyID: uuid.New()}

	svc := &fakeDigiLockerKYCSvc{}
	r := kycTestTenantRouterWithTenant(svc, "", tenant)
	req := httptest.NewRequest(http.MethodGet, "/api/tenant/kyc/digilocker/status", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if resp["status"] != "verified" || resp["method"] != "digilocker" {
		t.Fatalf("unexpected status response: %v", resp)
	}
}

func TestCashfreeKYCWebhook_DigiLocker_Success(t *testing.T) {
	secret := "test_webhook_secret_key_12345"
	svc := &fakeKYCSvc{}
	r := kycTestTenantRouter(svc, secret)
	body := `{"event_type":"DIGILOCKER_VERIFICATION_SUCCESS","data":{"verification_id":"ver_dgl_123","reference_id":998877,"status":"AUTHENTICATED"}}`
	nowTS := fmt.Sprintf("%d", time.Now().Unix())

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/kyc", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-webhook-timestamp", nowTS)
	req.Header.Set("x-webhook-signature", signTestHMAC(secret, nowTS+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["status"] != "received" || resp["state"] != "verified" {
		t.Fatalf("unexpected webhook response: %v", resp)
	}
}

func TestCashfreeKYCWebhook_DigiLocker_FailureEvents(t *testing.T) {
	secret := "test_webhook_secret_key_12345"

	events := []string{
		"DIGILOCKER_VERIFICATION_LINK_EXPIRED",
		"DIGILOCKER_VERIFICATION_CONSENT_DENIED",
		"DIGILOCKER_VERIFICATION_CONSENT_EXPIRED",
		"DIGILOCKER_VERIFICATION_FAILURE",
	}

	for _, evt := range events {
		t.Run(evt, func(t *testing.T) {
			svc := &fakeKYCSvc{}
			r := kycTestTenantRouter(svc, secret)
			body := fmt.Sprintf(`{"event_type":%q,"data":{"verification_id":"ver_fail_1","reference_id":112233,"status":"FAILED"}}`, evt)
			nowTS := fmt.Sprintf("%d", time.Now().Unix())

			req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/kyc", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("x-webhook-timestamp", nowTS)
			req.Header.Set("x-webhook-signature", signTestHMAC(secret, nowTS+body))
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("expected 200 for %s, got %d: %s", evt, w.Code, w.Body.String())
			}
		})
	}
}

func TestCashfreeKYCWebhook_DigiLocker_FlatPayload(t *testing.T) {
	secret := "test_webhook_secret_key_12345"
	svc := &fakeKYCSvc{}
	r := kycTestTenantRouter(svc, secret)
	body := `{"type":"DIGILOCKER_VERIFICATION_CONSENT_DENIED","verification_id":"ver_flat_456","reference_id":"456789","status":"DENIED"}`
	nowTS := fmt.Sprintf("%d", time.Now().Unix())

	req := httptest.NewRequest(http.MethodPost, "/webhooks/cashfree/kyc", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-webhook-timestamp", nowTS)
	req.Header.Set("x-webhook-signature", signTestHMAC(secret, nowTS+body))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}
