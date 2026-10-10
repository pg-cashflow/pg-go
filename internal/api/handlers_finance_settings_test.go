package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/finance"
)

func setupFinanceRouter(t *testing.T) (*gin.Engine, *domain.User, string, uuid.UUID) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	secret := "test-secret-with-sufficient-length-32-bytes!"
	propID := uuid.New()
	ownerUser := &domain.User{
		ID:         uuid.New(),
		Role:       domain.RoleOwner,
		Phone:      "+919876543210",
		PropertyID: &propID,
	}

	propStore := &scopeTestPropertyStore{props: map[uuid.UUID]*domain.Property{
		propID: {ID: propID, OwnerPhone: ownerUser.Phone},
	}}
	userStore := &scopeTestUserStore{users: map[uuid.UUID]*domain.User{
		ownerUser.ID: ownerUser,
	}}

	memStore := finance.NewMemoryStore()
	finSvc := finance.NewService(memStore, events.NoopPublisher{})

	r := NewRouter(Deps{
		JWTSecret:      secret,
		PropertyStore:  propStore,
		UserStore:      userStore,
		AuthUserRepo:   userStore,
		FinanceEnabled: true,
		Finance:        finSvc,
	})

	token, err := auth.IssueToken(secret, ownerUser)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	return r, ownerUser, token, propID
}

func TestHTTP_FinanceSettings_Get(t *testing.T) {
	r, _, token, propID := setupFinanceRouter(t)

	endpoints := []string{"/api/owner/finance/settings", "/api/owner/finance/policies"}
	for _, ep := range endpoints {
		t.Run(ep, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, ep, nil)
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-Property-ID", propID.String())
			w := httptest.NewRecorder()

			r.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
			}

			var resp struct {
				Settings *domain.PropertyFinanceSettings `json:"settings"`
				Policy   *domain.ApprovalPolicy          `json:"policy"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("failed to unmarshal response: %v", err)
			}
			if resp.Settings == nil || resp.Policy == nil {
				t.Fatalf("expected non-nil settings and policy")
			}
			if resp.Policy.EmergencyBypassEnabled != true {
				t.Errorf("expected default EmergencyBypassEnabled to be true")
			}
		})
	}
}

func TestHTTP_FinanceSettings_PatchPartialPreservesFields(t *testing.T) {
	r, _, token, propID := setupFinanceRouter(t)

	// Update only single_expense_limit_paise
	body := map[string]any{
		"policy": map[string]any{
			"single_expense_limit_paise": 750000,
		},
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest(http.MethodPatch, "/api/owner/finance/settings", bytes.NewReader(bodyBytes))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Property-ID", propID.String())
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Settings *domain.PropertyFinanceSettings `json:"settings"`
		Policy   *domain.ApprovalPolicy          `json:"policy"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if resp.Policy.SingleExpenseLimitPaise != 750000 {
		t.Errorf("SingleExpenseLimitPaise = %d, want 750000", resp.Policy.SingleExpenseLimitPaise)
	}
	// Verify other limits and booleans were NOT zeroed out
	if resp.Policy.ManagerDailyLimitPaise != 1_000_000 {
		t.Errorf("ManagerDailyLimitPaise was zeroed: %d, want 1000000", resp.Policy.ManagerDailyLimitPaise)
	}
	if resp.Policy.EmergencyBypassEnabled != true {
		t.Errorf("EmergencyBypassEnabled was disabled, want true")
	}
}

func TestHTTP_FinanceSettings_PatchValidationErrors(t *testing.T) {
	r, _, token, propID := setupFinanceRouter(t)

	cases := []struct {
		name string
		body map[string]any
	}{
		{
			name: "negative daily limit",
			body: map[string]any{
				"policy": map[string]any{
					"manager_daily_limit_paise": -500,
				},
			},
		},
		{
			name: "oversized limit",
			body: map[string]any{
				"policy": map[string]any{
					"manager_daily_limit_paise": 200000000000,
				},
			},
		},
		{
			name: "invalid fiscal day",
			body: map[string]any{
				"settings": map[string]any{
					"fiscal_month_start_day": 0,
				},
			},
		},
		{
			name: "fiscal day exceeds 31",
			body: map[string]any{
				"settings": map[string]any{
					"fiscal_month_start_day": 32,
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bodyBytes, _ := json.Marshal(tc.body)
			req := httptest.NewRequest(http.MethodPatch, "/api/owner/finance/settings", bytes.NewReader(bodyBytes))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("X-Property-ID", propID.String())
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			r.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected status 400 Bad Request, got %d: %s", w.Code, w.Body.String())
			}

			var env apierr.ErrorEnvelope
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("unmarshal error envelope: %v", err)
			}
			if env.Code != apierr.CodeFinanceInvalidSettings {
				t.Errorf("code = %q, want %q", env.Code, apierr.CodeFinanceInvalidSettings)
			}
		})
	}
}
