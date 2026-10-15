package api_test

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
	"github.com/pg-cashflow/pg-go/internal/api"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	joinsvc "github.com/pg-cashflow/pg-go/internal/join"
)

type fakeAuthRefreshService struct {
	stubPIIAuthService
	rotateErr error
	otpErr    error
	otpCalls  []string
}

func (s *fakeAuthRefreshService) RotateRefreshToken(_ context.Context, token string) (string, string, *domain.User, error) {
	if s.rotateErr != nil {
		return "", "", nil, s.rotateErr
	}
	user := &domain.User{ID: uuid.New(), Role: domain.RoleOwner}
	return "fresh-access-token", "rotated-refresh-token", user, nil
}

func (s *fakeAuthRefreshService) RequestOTP(_ context.Context, phone string) error {
	s.otpCalls = append(s.otpCalls, phone)
	return s.otpErr
}

type fakeAuthJoinService struct {
	properties map[string]*domain.Property
	joins      map[uuid.UUID]*domain.JoinRequest
}

func (s *fakeAuthJoinService) LookupInvite(_ context.Context, code string) (*domain.Property, error) {
	if p, ok := s.properties[code]; ok {
		return p, nil
	}
	return nil, errors.New("invalid invite code")
}

func (s *fakeAuthJoinService) RotateInvite(_ context.Context, _ uuid.UUID) (string, error) {
	return "", nil
}

func (s *fakeAuthJoinService) EnsurePending(_ context.Context, _ *domain.User, _ uuid.UUID, _ *uuid.UUID) (*domain.JoinRequest, error) {
	return nil, nil
}

func (s *fakeAuthJoinService) SetProfile(_ context.Context, _ uuid.UUID, _ string, _ *string) (*domain.JoinRequest, error) {
	return nil, nil
}

func (s *fakeAuthJoinService) CompleteOnboarding(_ context.Context, _ uuid.UUID, _ joinsvc.ProfileInput) (*domain.JoinRequest, *domain.Tenant, error) {
	return nil, nil, nil
}

func (s *fakeAuthJoinService) Me(_ context.Context, userID uuid.UUID) (*domain.JoinRequest, error) {
	if j, ok := s.joins[userID]; ok {
		return j, nil
	}
	return nil, errors.New("join request not found")
}

func (s *fakeAuthJoinService) List(_ context.Context, _ uuid.UUID, _ *domain.JoinStatus) ([]domain.JoinRequest, error) {
	return nil, nil
}

func (s *fakeAuthJoinService) Activate(_ context.Context, _, _ uuid.UUID, _ joinsvc.ActivateInput) (*domain.Tenant, error) {
	return nil, nil
}

func (s *fakeAuthJoinService) Reject(_ context.Context, _, _ uuid.UUID) error {
	return nil
}

func (s *fakeAuthJoinService) BuildOnboardingState(_ context.Context, _ *domain.User, _ bool) (*joinsvc.OnboardingState, error) {
	return &joinsvc.OnboardingState{Stage: "profile"}, nil
}

// TestAuthHandlers_T4d tests AuthRefresh, OTPRequest, LookupInvite, and JoinMe (Ticket T4d)
func TestAuthHandlers_T4d(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test-secret-key-with-sufficient-length-32"

	authSvc := &fakeAuthRefreshService{}
	joinSvc := &fakeAuthJoinService{
		properties: make(map[string]*domain.Property),
		joins:      make(map[uuid.UUID]*domain.JoinRequest),
	}

	h := &api.Handlers{
		Deps: api.Deps{
			JWTSecret: jwtSecret,
			Auth:      authSvc,
			Joins:     joinSvc,
		},
	}

	router := gin.New()
	router.POST("/auth/refresh", h.AuthRefresh)
	router.POST("/auth/otp/request", h.OTPRequest)
	router.GET("/join/invite/:code", h.LookupInvite)

	pendingGroup := router.Group("/join", auth.RequirePendingJoin(jwtSecret, nil))
	{
		pendingGroup.GET("/me", h.JoinMe)
	}

	t.Run("AuthRefresh: Missing cookie returns 401", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for missing refresh token, got %d", w.Code)
		}
	})

	t.Run("AuthRefresh: Valid refresh token cookie returns 200 and rotates cookie", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
		req.AddCookie(&http.Cookie{
			Name:  api.RefreshCookieName,
			Value: "valid-active-refresh-token",
		})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		cookieHeader := w.Header().Get("Set-Cookie")
		if !strings.Contains(cookieHeader, "rotated-refresh-token") {
			t.Fatalf("expected rotated-refresh-token in Set-Cookie, got: %s", cookieHeader)
		}
	})

	t.Run("AuthRefresh: Replayed token returns 401 with CodeAuthAccessRevoked and clears cookie", func(t *testing.T) {
		authSvc.rotateErr = auth.ErrReplayDetected
		defer func() { authSvc.rotateErr = nil }()

		req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
		req.AddCookie(&http.Cookie{
			Name:  api.RefreshCookieName,
			Value: "replayed-stale-refresh-token",
		})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for replayed token, got %d body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), string(apierr.CodeAuthAccessRevoked)) {
			t.Fatalf("expected CodeAuthAccessRevoked in body, got: %s", w.Body.String())
		}
		cookieHeader := w.Header().Get("Set-Cookie")
		if !strings.Contains(cookieHeader, "Max-Age=0") && !strings.Contains(cookieHeader, "expires=") {
			t.Fatalf("expected cookie cleared on replay, got Set-Cookie: %s", cookieHeader)
		}
	})

	t.Run("AuthRefresh: Revoked or expired session returns 401 and clears cookie", func(t *testing.T) {
		authSvc.rotateErr = auth.ErrInvalidToken
		defer func() { authSvc.rotateErr = nil }()

		req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
		req.AddCookie(&http.Cookie{
			Name:  api.RefreshCookieName,
			Value: "expired-or-revoked-token",
		})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401 for revoked session, got %d body=%s", w.Code, w.Body.String())
		}
		cookieHeader := w.Header().Get("Set-Cookie")
		if !strings.Contains(cookieHeader, "Max-Age=0") && !strings.Contains(cookieHeader, "expires=") {
			t.Fatalf("expected cookie cleared, got Set-Cookie: %s", cookieHeader)
		}
	})

	t.Run("AuthRefresh: Tenant vacated returns 403 Forbidden with CodeAuthAccessRevoked", func(t *testing.T) {
		authSvc.rotateErr = auth.ErrTenantVacated
		defer func() { authSvc.rotateErr = nil }()

		req := httptest.NewRequest(http.MethodPost, "/auth/refresh", nil)
		req.AddCookie(&http.Cookie{
			Name:  api.RefreshCookieName,
			Value: "vacated-tenant-token",
		})
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for vacated tenant, got %d body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), string(apierr.CodeAuthAccessRevoked)) {
			t.Fatalf("expected CodeAuthAccessRevoked in body, got: %s", w.Body.String())
		}
	})

	t.Run("OTPRequest: Malformed body returns 400 Bad Request", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/auth/otp/request", bytes.NewReader([]byte(`invalid-json`)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for malformed json, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("OTPRequest: Known and unknown phones return same 200 response (anti-enumeration)", func(t *testing.T) {
		authSvc.otpErr = nil

		// Known phone
		reqKnown := httptest.NewRequest(http.MethodPost, "/auth/otp/request", bytes.NewReader([]byte(`{"phone":"+919876543210"}`)))
		reqKnown.Header.Set("Content-Type", "application/json")
		wKnown := httptest.NewRecorder()
		router.ServeHTTP(wKnown, reqKnown)
		if wKnown.Code != http.StatusOK {
			t.Fatalf("expected 200 for known phone, got %d body=%s", wKnown.Code, wKnown.Body.String())
		}

		// Unknown phone
		reqUnknown := httptest.NewRequest(http.MethodPost, "/auth/otp/request", bytes.NewReader([]byte(`{"phone":"+919999999999"}`)))
		reqUnknown.Header.Set("Content-Type", "application/json")
		wUnknown := httptest.NewRecorder()
		router.ServeHTTP(wUnknown, reqUnknown)
		if wUnknown.Code != http.StatusOK {
			t.Fatalf("expected 200 for unknown phone, got %d body=%s", wUnknown.Code, wUnknown.Body.String())
		}

		// Verify identical body structure
		if wKnown.Body.String() != wUnknown.Body.String() {
			t.Fatalf("expected identical response bodies for anti-enumeration, got %q vs %q", wKnown.Body.String(), wUnknown.Body.String())
		}
	})

	t.Run("OTPRequest: Rate limited phone returns 429", func(t *testing.T) {
		authSvc.otpErr = auth.ErrRateLimited
		defer func() { authSvc.otpErr = nil }()

		req := httptest.NewRequest(http.MethodPost, "/auth/otp/request", bytes.NewReader([]byte(`{"phone":"+919876543210"}`)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusTooManyRequests {
			t.Fatalf("expected 429 for rate limited request, got %d body=%s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), string(apierr.CodeAuthRateLimited)) {
			t.Fatalf("expected CodeAuthRateLimited, got %s", w.Body.String())
		}
	})

	t.Run("LookupInvite: Valid invite code returns 200 with property details", func(t *testing.T) {
		testPropID := uuid.New()
		joinSvc.properties["VALID123"] = &domain.Property{
			ID:        testPropID,
			Name:      "Sunrise PG",
			OwnerName: "Rajesh Kumar",
		}

		req := httptest.NewRequest(http.MethodGet, "/join/invite/VALID123", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for valid invite, got %d body=%s", w.Code, w.Body.String())
		}
		var resp struct {
			PropertyID   uuid.UUID `json:"property_id"`
			PropertyName string    `json:"property_name"`
			OwnerName    string    `json:"owner_name"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.PropertyID != testPropID || resp.PropertyName != "Sunrise PG" {
			t.Fatalf("unexpected property details: %+v", resp)
		}
	})

	t.Run("LookupInvite: Invalid invite code returns 404", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/join/invite/UNKNOWN999", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for unknown invite, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("JoinMe: Pending tenant retrieves join status", func(t *testing.T) {
		pendingUserID := uuid.New()
		pendingPropID := uuid.New()

		pendingUser := &domain.User{
			ID:         pendingUserID,
			Role:       domain.RoleTenant,
			TenantID:   nil, // pending activation
			PropertyID: &pendingPropID,
		}
		pendingToken, err := auth.IssueToken(jwtSecret, pendingUser)
		if err != nil {
			t.Fatalf("failed to issue pending token: %v", err)
		}

		joinSvc.joins[pendingUserID] = &domain.JoinRequest{
			ID:         uuid.New(),
			UserID:     pendingUserID,
			PropertyID: pendingPropID,
			Phone:      "+919876543210",
			Status:     domain.JoinPending,
		}

		req := httptest.NewRequest(http.MethodGet, "/join/me", nil)
		req.Header.Set("Authorization", "Bearer "+pendingToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for JoinMe, got %d body=%s", w.Code, w.Body.String())
		}
		var resp struct {
			Join *domain.JoinRequest `json:"join"`
			User struct {
				ID   uuid.UUID   `json:"id"`
				Role domain.Role `json:"role"`
			} `json:"user"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if resp.Join == nil || resp.User.ID != pendingUserID {
			t.Fatalf("unexpected JoinMe payload: %+v", resp)
		}
	})

	t.Run("JoinMe: Missing join request returns 404", func(t *testing.T) {
		missingUserID := uuid.New()
		pendingPropID := uuid.New()

		missingUser := &domain.User{
			ID:         missingUserID,
			Role:       domain.RoleTenant,
			TenantID:   nil,
			PropertyID: &pendingPropID,
		}
		missingToken, err := auth.IssueToken(jwtSecret, missingUser)
		if err != nil {
			t.Fatalf("failed to issue pending token: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/join/me", nil)
		req.Header.Set("Authorization", "Bearer "+missingToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusNotFound {
			t.Fatalf("expected 404 for unprofiled join, got %d body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("JoinMe: Already activated tenant returns 403 Forbidden", func(t *testing.T) {
		activeTenantID := uuid.New()
		activePropID := uuid.New()
		activeUser := &domain.User{
			ID:         uuid.New(),
			Role:       domain.RoleTenant,
			TenantID:   &activeTenantID, // already activated!
			PropertyID: &activePropID,
		}
		activeToken, _ := auth.IssueToken(jwtSecret, activeUser)

		req := httptest.NewRequest(http.MethodGet, "/join/me", nil)
		req.Header.Set("Authorization", "Bearer "+activeToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for already activated tenant calling /join/me, got %d", w.Code)
		}
	})
}
