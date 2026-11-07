package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/crypto"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockBankVerifier struct {
	res *cashfree.BankAccountVerificationResult
	err error
}

func (m *mockBankVerifier) VerifyBankAccountSync(_ context.Context, _ cashfree.BankAccountVerificationRequest) (*cashfree.BankAccountVerificationResult, error) {
	return m.res, m.err
}

func TestTenantRegisterRefundAccount_Validation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	propID := uuid.New()

	h := &Handlers{}
	r := gin.New()
	r.POST("/tenant/refund-account", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, &domain.Tenant{
			ID:         tenantID,
			PropertyID: propID,
			Name:       "John Doe",
		})
		h.TenantRegisterRefundAccount(c)
	})

	tests := []struct {
		name string
		body map[string]string
		code int
	}{
		{"missing fields", map[string]string{}, http.StatusBadRequest},
		{"short account", map[string]string{"account_number": "123", "ifsc": "HDFC0001234"}, http.StatusBadRequest},
		{"invalid ifsc", map[string]string{"account_number": "1234567890", "ifsc": "INVALID_IFSC"}, http.StatusBadRequest},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(http.MethodPost, "/tenant/refund-account", bytes.NewReader(b))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			if w.Code != tc.code {
				t.Fatalf("expected status %d, got %d (body: %s)", tc.code, w.Code, w.Body.String())
			}
		})
	}
}

func TestTenantRegisterRefundAccount_BankResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	propID := uuid.New()

	t.Run("invalid bank account rejected", func(t *testing.T) {
		mockVer := &mockBankVerifier{
			res: &cashfree.BankAccountVerificationResult{
				AccountStatus:     "INVALID",
				AccountStatusCode: "INVALID_BANK_ACCOUNT",
			},
		}

		h := &Handlers{
			Deps: Deps{
				BankVerifier: mockVer,
			},
		}

		r := gin.New()
		r.POST("/tenant/refund-account", func(c *gin.Context) {
			c.Set(auth.ContextTenantKey, &domain.Tenant{
				ID:         tenantID,
				PropertyID: propID,
				Name:       "John Doe",
			})
			h.TenantRegisterRefundAccount(c)
		})

		body, _ := json.Marshal(map[string]string{
			"account_number": "1234567890",
			"ifsc":           "HDFC0001234",
		})
		req := httptest.NewRequest(http.MethodPost, "/tenant/refund-account", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422, got %d", w.Code)
		}
		var env apierr.ErrorEnvelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Code != apierr.CodeBankAccountInvalid {
			t.Fatalf("expected code %s, got %s", apierr.CodeBankAccountInvalid, env.Code)
		}
	})

	t.Run("name mismatch rejected", func(t *testing.T) {
		mockVer := &mockBankVerifier{
			res: &cashfree.BankAccountVerificationResult{
				AccountStatus:     "VALID",
				AccountStatusCode: "ACCOUNT_IS_VALID",
				NameMatchResult:   "POOR_PARTIAL_MATCH",
				NameMatchScore:    "30.00",
			},
		}

		h := &Handlers{
			Deps: Deps{
				BankVerifier: mockVer,
			},
		}

		r := gin.New()
		r.POST("/tenant/refund-account", func(c *gin.Context) {
			c.Set(auth.ContextTenantKey, &domain.Tenant{
				ID:         tenantID,
				PropertyID: propID,
				Name:       "John Doe",
			})
			h.TenantRegisterRefundAccount(c)
		})

		body, _ := json.Marshal(map[string]string{
			"account_number": "1234567890",
			"ifsc":           "HDFC0001234",
		})
		req := httptest.NewRequest(http.MethodPost, "/tenant/refund-account", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("expected 422, got %d", w.Code)
		}
		var env apierr.ErrorEnvelope
		_ = json.Unmarshal(w.Body.Bytes(), &env)
		if env.Code != apierr.CodeBankNameMismatch {
			t.Fatalf("expected code %s, got %s", apierr.CodeBankNameMismatch, env.Code)
		}
	})
}

func TestIsAcceptableNameMatch(t *testing.T) {
	tests := []struct {
		name   string
		result string
		score  string
		want   bool
	}{
		{"direct match accepted", "DIRECT_MATCH", "100.00", true},
		{"good partial match accepted", "GOOD_PARTIAL_MATCH", "85.00", true},
		{"moderate partial match not auto-accepted (high score)", "MODERATE_PARTIAL_MATCH", "75.00", false},
		{"moderate partial match not auto-accepted (low score)", "MODERATE_PARTIAL_MATCH", "55.00", false},
		{"poor partial match rejected", "POOR_PARTIAL_MATCH", "30.00", false},
		{"no match rejected", "NO_MATCH", "0.00", false},
		{"empty result with high score accepted", "", "90.00", true},
		{"empty result with moderate score rejected", "", "65.00", false},
		{"empty result with low score rejected", "", "45.00", false},
		{"empty result with zero score rejected", "", "0.00", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAcceptableNameMatch(tc.result, tc.score); got != tc.want {
				t.Errorf("isAcceptableNameMatch(%q, %q) = %v, want %v", tc.result, tc.score, got, tc.want)
			}
		})
	}
}

func TestSanitizeBankVerificationName(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"D'Souza", "D Souza"},
		{"O'Connor-Smith", "O Connor-Smith"},
		{"John & Sons / Co.", "John & Sons / Co."},
		{"Dr. Rajesh-Kumar", "Dr. Rajesh-Kumar"},
		{"Vijay (PG Tenant)!", "Vijay PG Tenant"},
		{"  Rahul   Sharma  ", "Rahul Sharma"},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			if got := SanitizeBankVerificationName(tc.in); got != tc.want {
				t.Errorf("SanitizeBankVerificationName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestTenantRegisterRefundAccount_GatewayStatusCodes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	propID := uuid.New()

	tests := []struct {
		name       string
		res        *cashfree.BankAccountVerificationResult
		wantStatus int
		wantCode   apierr.Code
	}{
		{
			name: "insufficient merchant balance returns 503",
			res: &cashfree.BankAccountVerificationResult{
				AccountStatus:     "INVALID",
				AccountStatusCode: "INSUFFICIENT_BALANCE",
				Message:           "Low balance in merchant wallet",
			},
			wantStatus: http.StatusServiceUnavailable,
			wantCode:   apierr.CodeBankVerifierUnavailable,
		},
		{
			name: "invalid ifsc returns 422 with invalid body code",
			res: &cashfree.BankAccountVerificationResult{
				AccountStatus:     "INVALID",
				AccountStatusCode: "INVALID_IFSC",
				Message:           "The IFSC provided is invalid",
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   apierr.CodeRequestInvalidBody,
		},
		{
			name: "account blocked returns 422 with invalid bank account code",
			res: &cashfree.BankAccountVerificationResult{
				AccountStatus:     "INVALID",
				AccountStatusCode: "ACCOUNT_BLOCKED",
				Message:           "Account has been frozen or blocked",
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   apierr.CodeBankAccountInvalid,
		},
		{
			name: "unsupported bank returns 422 with invalid bank account code",
			res: &cashfree.BankAccountVerificationResult{
				AccountStatus:     "INVALID",
				AccountStatusCode: "BANK_UNSUPPORTED",
				Message:           "Bank does not support verification",
			},
			wantStatus: http.StatusUnprocessableEntity,
			wantCode:   apierr.CodeBankAccountInvalid,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mockVer := &mockBankVerifier{res: tc.res}
			h := &Handlers{Deps: Deps{BankVerifier: mockVer}}

			r := gin.New()
			r.POST("/tenant/refund-account", func(c *gin.Context) {
				c.Set(auth.ContextTenantKey, &domain.Tenant{
					ID:         tenantID,
					PropertyID: propID,
					Name:       "Test Tenant",
				})
				h.TenantRegisterRefundAccount(c)
			})

			body, _ := json.Marshal(map[string]string{
				"account_number": "1234567890",
				"ifsc":           "HDFC0001234",
			})
			req := httptest.NewRequest(http.MethodPost, "/tenant/refund-account", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != tc.wantStatus {
				t.Fatalf("expected HTTP status %d, got %d (body: %s)", tc.wantStatus, w.Code, w.Body.String())
			}
			var env apierr.ErrorEnvelope
			_ = json.Unmarshal(w.Body.Bytes(), &env)
			if env.Code != tc.wantCode {
				t.Fatalf("expected error code %s, got %s", tc.wantCode, env.Code)
			}
		})
	}
}

func TestTenantRegisterRefundAccount_Sanitized502(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	propID := uuid.New()

	mockVer := &mockBankVerifier{
		err: errors.New("dial tcp 10.0.0.1:443: connect: connection refused to https://api.cashfree.com/verification"),
	}
	h := &Handlers{Deps: Deps{BankVerifier: mockVer}}

	r := gin.New()
	r.POST("/tenant/refund-account", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, &domain.Tenant{
			ID:         tenantID,
			PropertyID: propID,
			Name:       "Test Tenant",
		})
		h.TenantRegisterRefundAccount(c)
	})

	body, _ := json.Marshal(map[string]string{
		"account_number": "1234567890",
		"ifsc":           "HDFC0001234",
	})
	req := httptest.NewRequest(http.MethodPost, "/tenant/refund-account", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected HTTP 502, got %d", w.Code)
	}
	bodyStr := w.Body.String()
	if strings.Contains(bodyStr, "https://api.cashfree.com") || strings.Contains(bodyStr, "dial tcp") {
		t.Fatalf("502 response leaked internal error details: %s", bodyStr)
	}
}

func TestTenantRegisterRefundAccount_KYCRequired(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	propID := uuid.New()

	unverifiedSvc := &fakeKYCSvc{
		statusFn: func(_ context.Context, _ uuid.UUID) (*domain.KYCVerification, *domain.KYCConsent, error) {
			return &domain.KYCVerification{Status: domain.KYCStatusPendingManualReview}, nil, nil
		},
	}
	h := &Handlers{Deps: Deps{KYCSvc: unverifiedSvc}}

	r := gin.New()
	r.POST("/tenant/refund-account", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, &domain.Tenant{
			ID:         tenantID,
			PropertyID: propID,
			Name:       "Test Tenant",
		})
		h.TenantRegisterRefundAccount(c)
	})

	body, _ := json.Marshal(map[string]string{
		"account_number": "1234567890",
		"ifsc":           "HDFC0001234",
	})
	req := httptest.NewRequest(http.MethodPost, "/tenant/refund-account", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected HTTP 403 when tenant is not KYC verified, got %d (body: %s)", w.Code, w.Body.String())
	}
	var env apierr.ErrorEnvelope
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	if env.Code != apierr.CodeAuthForbidden {
		t.Fatalf("expected error code %s, got %s", apierr.CodeAuthForbidden, env.Code)
	}
}

func TestTenantGetRefundAccount_Empty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	propID := uuid.New()

	h := &Handlers{}
	r := gin.New()
	r.GET("/tenant/refund-account", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, &domain.Tenant{
			ID:         tenantID,
			PropertyID: propID,
		})
		h.TenantGetRefundAccount(c)
	})

	req := httptest.NewRequest(http.MethodGet, "/tenant/refund-account", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Since PayoutRepo is nil, returns 503
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 when PayoutRepo is nil, got %d", w.Code)
	}
}

func TestSetupKeyRingForTest(t *testing.T) {
	key := []byte("01234567890123456789012345678901") // 32 bytes
	kr, err := crypto.NewKeyRing(1, key)
	if err != nil {
		t.Fatalf("keyring init: %v", err)
	}
	if kr == nil {
		t.Fatal("expected non-nil keyring")
	}
}
