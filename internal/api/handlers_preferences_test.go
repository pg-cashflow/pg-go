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
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/localization"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type stubPreferencesStore struct {
	prefs map[uuid.UUID]string
}

func (s *stubPreferencesStore) GetByUserID(_ context.Context, userID uuid.UUID) (*domain.UserPreferences, error) {
	loc, ok := s.prefs[userID]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return &domain.UserPreferences{
		UserID: userID,
		Locale: loc,
	}, nil
}

func (s *stubPreferencesStore) Upsert(_ context.Context, userID uuid.UUID, locale string) (*domain.UserPreferences, error) {
	if !localization.IsValid(locale) {
		return nil, postgres.ErrInvalidLocale
	}
	if s.prefs == nil {
		s.prefs = make(map[uuid.UUID]string)
	}
	s.prefs[userID] = locale
	return &domain.UserPreferences{
		UserID: userID,
		Locale: locale,
	}, nil
}

func TestPreferencesEndpoints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test-secret-key-with-sufficient-length-32"
	propID := uuid.New()
	userID := uuid.New()
	user := &domain.User{
		ID:           userID,
		Role:         domain.RoleOwner,
		PropertyID:   &propID,
		TokenVersion: 1,
	}
	token, err := auth.IssueToken(jwtSecret, user)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	sessionStore := &stubSessionStore{user: user}
	prefStore := &stubPreferencesStore{prefs: make(map[uuid.UUID]string)}

	router := NewRouter(Deps{
		JWTSecret:        jwtSecret,
		AuthUserRepo:     sessionStore,
		PreferencesStore: prefStore,
	})

	// 1. GET /api/locales (public)
	{
		req := httptest.NewRequest(http.MethodGet, "/api/locales", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/locales expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Locales       []localization.LocaleMetadata `json:"locales"`
			DefaultLocale string                        `json:"default_locale"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal /api/locales: %v", err)
		}
		if len(resp.Locales) != 4 {
			t.Errorf("expected 4 locales, got %d", len(resp.Locales))
		}
		if resp.DefaultLocale != "en-IN" {
			t.Errorf("expected default en-IN, got %s", resp.DefaultLocale)
		}
	}

	// 2. GET /api/me/preferences unauthorized
	{
		req := httptest.NewRequest(http.MethodGet, "/api/me/preferences", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", rec.Code)
		}
	}

	// 3. GET /api/me/preferences authorized with fallback and Accept-Language
	{
		req := httptest.NewRequest(http.MethodGet, "/api/me/preferences", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept-Language", "te-IN")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Locale             string `json:"locale"`
			HasSavedPreference bool   `json:"has_saved_preference"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Locale != "te-IN" {
			t.Errorf("expected Accept-Language te-IN to resolve, got %s", resp.Locale)
		}
		if resp.HasSavedPreference {
			t.Errorf("expected HasSavedPreference false before explicit save")
		}
	}

	// 4. PATCH /api/me/preferences invalid locale
	{
		body, _ := json.Marshal(map[string]string{"locale": "invalid-locale"})
		req := httptest.NewRequest(http.MethodPatch, "/api/me/preferences", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
		var errResp struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &errResp); err != nil {
			t.Fatalf("unmarshal error resp: %v", err)
		}
		if errResp.Code != "preferences.invalidLocale" {
			t.Errorf("expected code preferences.invalidLocale, got %s", errResp.Code)
		}
	}

	// 5. PATCH /api/me/preferences valid locale
	{
		body, _ := json.Marshal(map[string]string{"locale": "kn-IN"})
		req := httptest.NewRequest(http.MethodPatch, "/api/me/preferences", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Locale             string `json:"locale"`
			HasSavedPreference bool   `json:"has_saved_preference"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Locale != "kn-IN" {
			t.Errorf("expected kn-IN, got %s", resp.Locale)
		}
		if !resp.HasSavedPreference {
			t.Errorf("expected HasSavedPreference true after explicit save")
		}
	}

	// 6. GET /api/me/preferences now returns updated locale and HasSavedPreference=true
	{
		req := httptest.NewRequest(http.MethodGet, "/api/me/preferences", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		// Even if Accept-Language is different, explicit preference wins
		req.Header.Set("Accept-Language", "te-IN")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Locale             string `json:"locale"`
			HasSavedPreference bool   `json:"has_saved_preference"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if resp.Locale != "kn-IN" {
			t.Errorf("expected kn-IN, got %s", resp.Locale)
		}
		if !resp.HasSavedPreference {
			t.Errorf("expected HasSavedPreference true")
		}
	}
}
