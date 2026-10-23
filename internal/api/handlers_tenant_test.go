package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type stubPatchTenantStore struct {
	t         *domain.Tenant
	updateErr error
	updates   int
}

func (s *stubPatchTenantStore) GetByID(_ context.Context, _ uuid.UUID) (*domain.Tenant, error) {
	return s.t, nil
}
func (s *stubPatchTenantStore) ListByProperty(_ context.Context, _ uuid.UUID) ([]domain.Tenant, error) {
	return nil, nil
}
func (s *stubPatchTenantStore) Update(_ context.Context, t *domain.Tenant) error {
	s.updates++
	s.t = t
	return s.updateErr
}
func (s *stubPatchTenantStore) GetIDPhoto(_ context.Context, _ uuid.UUID) ([]byte, error) {
	return nil, nil
}

type stubPatchPreferencesStore struct {
	upsertFunc func(ctx context.Context, userID uuid.UUID, locale *string, theme *string) (*domain.UserPreferences, error)
	lastLocale *string
}

func (s *stubPatchPreferencesStore) GetByUserID(_ context.Context, _ uuid.UUID) (*domain.UserPreferences, error) {
	return nil, nil
}
func (s *stubPatchPreferencesStore) Upsert(ctx context.Context, userID uuid.UUID, locale *string, theme *string) (*domain.UserPreferences, error) {
	s.lastLocale = locale
	if s.upsertFunc != nil {
		return s.upsertFunc(ctx, userID, locale, theme)
	}
	loc := ""
	if locale != nil {
		loc = *locale
	}
	return &domain.UserPreferences{UserID: userID, Locale: loc}, nil
}

func TestTenantPatchMe_ValidNameUpdate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	userID := uuid.New()
	ten := &domain.Tenant{ID: tenantID, PropertyID: uuid.New(), Name: "Old Name"}
	tStore := &stubPatchTenantStore{t: ten}
	h := &Handlers{Deps: Deps{TenantStore: tStore}}

	r := gin.New()
	r.PATCH("/tenant/me", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, ten)
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: userID, Role: domain.RoleTenant})
		h.TenantPatchMe(c)
	})

	body := `{"name":"  New Name  "}`
	req := httptest.NewRequest(http.MethodPatch, "/tenant/me", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if ten.Name != "New Name" {
		t.Fatalf("expected trimmed name 'New Name', got '%s'", ten.Name)
	}
	if tStore.updates != 1 {
		t.Fatalf("expected 1 update, got %d", tStore.updates)
	}
}

func TestTenantPatchMe_EmptyNameRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	userID := uuid.New()
	ten := &domain.Tenant{ID: tenantID, PropertyID: uuid.New(), Name: "Initial Name"}
	tStore := &stubPatchTenantStore{t: ten}
	h := &Handlers{Deps: Deps{TenantStore: tStore}}

	r := gin.New()
	r.PATCH("/tenant/me", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, ten)
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: userID, Role: domain.RoleTenant})
		h.TenantPatchMe(c)
	})

	body := `{"name":"   "}`
	req := httptest.NewRequest(http.MethodPatch, "/tenant/me", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty name, got %d: %s", w.Code, w.Body.String())
	}
	if tStore.updates != 0 {
		t.Fatalf("expected 0 updates on validation failure, got %d", tStore.updates)
	}
}

func TestTenantPatchMe_GuardianPhoneUpdate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	userID := uuid.New()
	ten := &domain.Tenant{ID: tenantID, PropertyID: uuid.New()}
	tStore := &stubPatchTenantStore{t: ten}
	h := &Handlers{Deps: Deps{TenantStore: tStore}}

	r := gin.New()
	r.PATCH("/tenant/me", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, ten)
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: userID, Role: domain.RoleTenant})
		h.TenantPatchMe(c)
	})

	body := `{"guardian_phone":"  +919876543210  "}`
	req := httptest.NewRequest(http.MethodPatch, "/tenant/me", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if ten.GuardianPhone == nil || *ten.GuardianPhone != "+919876543210" {
		t.Fatalf("expected trimmed phone '+919876543210', got %v", ten.GuardianPhone)
	}
}

func TestTenantPatchMe_ValidLocaleUpdate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	userID := uuid.New()
	ten := &domain.Tenant{ID: tenantID, PropertyID: uuid.New()}
	tStore := &stubPatchTenantStore{t: ten}
	pStore := &stubPatchPreferencesStore{}
	h := &Handlers{Deps: Deps{TenantStore: tStore, PreferencesStore: pStore}}

	r := gin.New()
	r.PATCH("/tenant/me", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, ten)
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: userID, Role: domain.RoleTenant})
		h.TenantPatchMe(c)
	})

	body := `{"locale":"te-IN"}`
	req := httptest.NewRequest(http.MethodPatch, "/tenant/me", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if pStore.lastLocale == nil || *pStore.lastLocale != "te-IN" {
		t.Fatalf("expected locale 'te-IN' saved to preferences, got %v", pStore.lastLocale)
	}
}

func TestTenantPatchMe_InvalidLocaleRejection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	userID := uuid.New()
	ten := &domain.Tenant{ID: tenantID, PropertyID: uuid.New()}
	tStore := &stubPatchTenantStore{t: ten}
	pStore := &stubPatchPreferencesStore{}
	h := &Handlers{Deps: Deps{TenantStore: tStore, PreferencesStore: pStore}}

	r := gin.New()
	r.PATCH("/tenant/me", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, ten)
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: userID, Role: domain.RoleTenant})
		h.TenantPatchMe(c)
	})

	body := `{"locale":"fr-FR"}`
	req := httptest.NewRequest(http.MethodPatch, "/tenant/me", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsupported locale, got %d: %s", w.Code, w.Body.String())
	}

	var env apierr.ErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("failed to parse error envelope: %v", err)
	}
	if env.Code != apierr.CodePreferencesInvalidLocale {
		t.Fatalf("expected error code %s, got %s", apierr.CodePreferencesInvalidLocale, env.Code)
	}
	if pStore.lastLocale != nil {
		t.Fatalf("preferences Upsert must not be called on invalid locale, but got %v", pStore.lastLocale)
	}
}

func TestTenantPatchMe_UpsertErrorPropagated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	userID := uuid.New()
	ten := &domain.Tenant{ID: tenantID, PropertyID: uuid.New()}
	tStore := &stubPatchTenantStore{t: ten}
	pStore := &stubPatchPreferencesStore{
		upsertFunc: func(_ context.Context, _ uuid.UUID, _ *string, _ *string) (*domain.UserPreferences, error) {
			return nil, errors.New("db connection failure")
		},
	}
	h := &Handlers{Deps: Deps{TenantStore: tStore, PreferencesStore: pStore}}

	r := gin.New()
	r.PATCH("/tenant/me", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, ten)
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: userID, Role: domain.RoleTenant})
		h.TenantPatchMe(c)
	})

	body := `{"locale":"te-IN"}`
	req := httptest.NewRequest(http.MethodPatch, "/tenant/me", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when preferences Upsert fails, got %d: %s", w.Code, w.Body.String())
	}
	if tStore.updates != 0 {
		t.Fatalf("tenant update must not occur if preferences upsert failed, got %d updates", tStore.updates)
	}
}

func TestTenantPatchMe_TenantUpdateErrorPropagated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	tenantID := uuid.New()
	userID := uuid.New()
	ten := &domain.Tenant{ID: tenantID, PropertyID: uuid.New()}
	tStore := &stubPatchTenantStore{
		t:         ten,
		updateErr: errors.New("tenant store failure"),
	}
	h := &Handlers{Deps: Deps{TenantStore: tStore}}

	r := gin.New()
	r.PATCH("/tenant/me", func(c *gin.Context) {
		c.Set(auth.ContextTenantKey, ten)
		c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: userID, Role: domain.RoleTenant})
		h.TenantPatchMe(c)
	})

	body := `{"name":"Valid Name"}`
	req := httptest.NewRequest(http.MethodPatch, "/tenant/me", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 when TenantStore.Update fails, got %d: %s", w.Code, w.Body.String())
	}
}

func TestTenantPatchMe_MissingTenantContext(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handlers{}

	r := gin.New()
	r.PATCH("/tenant/me", h.TenantPatchMe)

	body := `{"name":"New Name"}`
	req := httptest.NewRequest(http.MethodPatch, "/tenant/me", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 when tenant context missing, got %d: %s", w.Code, w.Body.String())
	}
}
