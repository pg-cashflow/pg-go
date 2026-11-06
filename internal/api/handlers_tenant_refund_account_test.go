package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		result string
		score  string
		want   bool
	}{
		{"DIRECT_MATCH", "100.00", true},
		{"GOOD_PARTIAL_MATCH", "85.00", true},
		{"MODERATE_PARTIAL_MATCH", "75.00", true},
		{"MODERATE_PARTIAL_MATCH", "55.00", false},
		{"POOR_PARTIAL_MATCH", "30.00", false},
		{"NO_MATCH", "0.00", false},
		{"", "65.00", true},
		{"", "45.00", false},
	}

	for _, tc := range tests {
		if got := isAcceptableNameMatch(tc.result, tc.score); got != tc.want {
			t.Errorf("isAcceptableNameMatch(%q, %q) = %v, want %v", tc.result, tc.score, got, tc.want)
		}
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
