package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	kycsvc "github.com/pg-cashflow/pg-go/internal/kyc"
)

type mockKYCServiceWithUpstreamError struct {
	kycsvc.Service
	upstreamErr error
}

func (m *mockKYCServiceWithUpstreamError) InitiateAadhaarOTP(ctx context.Context, tenantID uuid.UUID, aadhaarNumber, actor string) (string, error) {
	return "", m.upstreamErr
}

func (m *mockKYCServiceWithUpstreamError) CompleteAadhaarOTP(ctx context.Context, tenantID uuid.UUID, refID, otp, actor string) (*domain.KYCVerification, error) {
	return nil, m.upstreamErr
}

func (m *mockKYCServiceWithUpstreamError) VerifySecureQR(ctx context.Context, tenantID uuid.UUID, qrPayload, actor string) (*domain.KYCVerification, error) {
	return nil, m.upstreamErr
}

// TestKYC_ProviderErrorSanitization verifies that raw upstream responses containing internal
// gateway errors, stack traces, or upstream error message bodies never reach the client.
func TestKYC_ProviderErrorSanitization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	propID := uuid.New()

	upstreamErr := &cashfree.APIError{
		StatusCode: http.StatusBadGateway,
		Status:     "502 Bad Gateway",
		Body:       `{"message":"internal cashfree cluster timeout at 10.0.4.12:9000","internal_code":"CF_ERR_9981"}`,
	}

	h := &Handlers{
		Deps: Deps{
			KYCSvc: &mockKYCServiceWithUpstreamError{upstreamErr: upstreamErr},
		},
	}

	r := gin.New()
	r.POST("/api/tenant/kyc/aadhaar/otp/request", func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     tenantID,
			PropertyID: &propID,
			Role:       domain.RoleTenant,
		})
		c.Set("tenant", &domain.Tenant{ID: tenantID, PropertyID: propID})
		h.TenantKYCAadhaarOTPRequest(c)
	})

	body := map[string]any{"aadhaar_number": "999999990019"} // Verhoeff valid
	buf, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/aadhaar/otp/request", bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 Bad Gateway, got %d: %s", w.Code, w.Body.String())
	}

	respBody := w.Body.String()
	if strings.Contains(respBody, "10.0.4.12") || strings.Contains(respBody, "CF_ERR_9981") {
		t.Fatalf("CRITICAL SECURITY DEFECT: leaked raw upstream error body to client: %s", respBody)
	}

	var parsed map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("failed to unmarshal JSON response: %v", err)
	}
	errMsg, _ := parsed["error"].(string)
	if !strings.Contains(errMsg, "upstream error") || strings.Contains(errMsg, "cluster timeout") {
		t.Fatalf("expected sanitized public error message, got %q", errMsg)
	}
}

// TestKYC_RouteBodyLimits verifies that oversized JSON payloads are bounded and rejected.
func TestKYC_RouteBodyLimits(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	propID := uuid.New()

	h := &Handlers{
		Deps: Deps{
			KYCSvc: &mockKYCServiceWithUpstreamError{},
		},
	}

	r := gin.New()
	r.POST("/api/tenant/kyc/aadhaar/otp/request", func(c *gin.Context) {
		c.Set(auth.ContextClaimsKey, &auth.Claims{
			UserID:     tenantID,
			PropertyID: &propID,
			Role:       domain.RoleTenant,
		})
		c.Set("tenant", &domain.Tenant{ID: tenantID, PropertyID: propID})
		h.TenantKYCAadhaarOTPRequest(c)
	})

	// Send an oversized payload (>4KB limit on OTP request)
	hugeString := strings.Repeat("A", 10*1024)
	payload := `{"aadhaar_number":"` + hugeString + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/tenant/kyc/aadhaar/otp/request", strings.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// MaxBytesReader causes binding to fail and respond with an invalid body error / 400
	if w.Code != http.StatusBadRequest && w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 400 or 413 for oversized body, got %d: %s", w.Code, w.Body.String())
	}
}
