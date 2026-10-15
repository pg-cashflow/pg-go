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
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

func TestOwnerPatchOnboardingSettings_InvalidJoinerBps(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "test-secret-key-with-sufficient-length-32"
	propA := uuid.New()
	owner := &domain.User{ID: uuid.New(), Role: domain.RoleOwner, Phone: "+919000000401", PropertyID: &propA}

	router := NewRouter(Deps{
		JWTSecret: secret,
		PropertyStore: &scopeTestPropertyStore{props: map[uuid.UUID]*domain.Property{
			propA: {ID: propA, OwnerPhone: owner.Phone},
		}},
		UserStore: &scopeTestUserStore{users: map[uuid.UUID]*domain.User{owner.ID: owner}},
		OnboardingSettings: &stubOnboardingSettingsStore{
			update: func(_ context.Context, _ uuid.UUID, patch postgres.OnboardingSettingsPatch) (*domain.PropertyOnboardingSettings, error) {
				return nil, patch.Validate()
			},
		},
	})

	tok, err := auth.IssueToken(secret, owner)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.NewBufferString(`{"referral_enabled":true,"joiner_discount_bps":20000}`)
	req := httptest.NewRequest(http.MethodPatch, "/api/owner/onboarding-settings", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Code != string(apierr.CodeOnboardingSettingsInvalid) {
		t.Fatalf("code %q want %s", env.Code, apierr.CodeOnboardingSettingsInvalid)
	}
}

func TestManagerGetOnboardingSettings_OK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "test-secret-key-with-sufficient-length-32"
	propA := uuid.New()
	manager := &domain.User{ID: uuid.New(), Role: domain.RoleManager, Phone: "+919000000402", PropertyID: &propA}

	router := NewRouter(Deps{
		JWTSecret: secret,
		PropertyStore: &scopeTestPropertyStore{props: map[uuid.UUID]*domain.Property{
			propA: {ID: propA, OwnerPhone: "+919000000499"},
		}},
		UserStore: &scopeTestUserStore{users: map[uuid.UUID]*domain.User{manager.ID: manager}},
		OnboardingSettings: &stubOnboardingSettingsStore{},
	})

	tok, err := auth.IssueToken(secret, manager)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/manager/onboarding-settings", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("manager GET: status %d body %s", w.Code, w.Body.String())
	}
}

func TestTenantGetOnboardingSettings_Forbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "test-secret-key-with-sufficient-length-32"
	propA := uuid.New()
	tenantID := uuid.New()
	tenant := &domain.User{ID: uuid.New(), Role: domain.RoleTenant, Phone: "+919000000403", PropertyID: &propA, TenantID: &tenantID}

	router := NewRouter(Deps{
		JWTSecret: secret,
		PropertyStore: &scopeTestPropertyStore{props: map[uuid.UUID]*domain.Property{
			propA: {ID: propA, OwnerPhone: "+919000000499"},
		}},
		UserStore:          &scopeTestUserStore{users: map[uuid.UUID]*domain.User{tenant.ID: tenant}},
		OnboardingSettings: &stubOnboardingSettingsStore{},
		AuthTenantRepo:     matrixTenantRepo{tenantID: tenantID, propertyID: propA},
	})

	tok, err := auth.IssueToken(secret, tenant)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/owner/onboarding-settings", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("tenant on owner route: status %d want 403", w.Code)
	}
}
