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
	rotateErr         error
	otpErr            error
	otpCalls          []string
	sessionsToReturn  []domain.ActiveSession
	revokedSessionIDs []uuid.UUID
}

func (s *fakeAuthRefreshService) RotateRefreshToken(_ context.Context, token string) (string, string, *domain.User, error) {
	if s.rotateErr != nil {
		return "", "", nil, s.rotateErr
	}
	user := &domain.User{ID: uuid.New(), Role: domain.RoleOwner}
	return "fresh-access-token", "rotated-refresh-token", user, nil
}

func (s *fakeAuthRefreshService) RotateRefreshTokenWithClient(ctx context.Context, token, _, _ string) (string, string, *domain.User, error) {
	return s.RotateRefreshToken(ctx, token)
}

func (s *fakeAuthRefreshService) RequestOTP(_ context.Context, phone string) error {
	s.otpCalls = append(s.otpCalls, phone)
	return s.otpErr
}

func (s *fakeAuthRefreshService) ListSessions(_ context.Context, _ uuid.UUID, currentFamilyID *uuid.UUID, _ string) ([]domain.ActiveSession, error) {
	res := make([]domain.ActiveSession, len(s.sessionsToReturn))
	copy(res, s.sessionsToReturn)
	for i := range res {
		if currentFamilyID != nil && res[i].ID == *currentFamilyID {
			res[i].IsCurrent = true
		}
	}
	return res, nil
}

func (s *fakeAuthRefreshService) RevokeDeviceSession(_ context.Context, _, familyID uuid.UUID) error {
	s.revokedSessionIDs = append(s.revokedSessionIDs, familyID)
	return nil
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

func (s *fakeAuthJoinService) BindSlotInvite(_ context.Context, _ uuid.UUID, _ uuid.UUID) error {
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

	authGroup := router.Group("/auth", auth.RequireOwnerOrManagerOrTenant(jwtSecret, nil))
	{
		authGroup.GET("/sessions", h.GetActiveSessions)
		authGroup.DELETE("/sessions/:id", h.DeleteSession)
	}

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

	// OTPRequest is disabled per ticket C.4: Firebase Phone Auth is the single
	// front door (docs/specs/auth-frontend-backend-contract.md §1.1). The
	// handler no longer binds the body, calls auth.Service, or applies rate
	// limiting — it returns 410 for every request, which is the full
	// contract now. The former "malformed body -> 400", "anti-enumeration ->
	// identical 200s", and "rate limited -> 429" subtests asserted behavior
	// of the retired code path and were removed rather than patched, since
	// none of those mechanisms execute anymore.
	t.Run("OTPRequest: disabled endpoint returns 410 regardless of body", func(t *testing.T) {
		for _, body := range [][]byte{
			[]byte(`{"phone":"+919876543210"}`),
			[]byte(`{"phone":"+919999999999"}`),
			[]byte(`invalid-json`),
			[]byte(``),
		} {
			req := httptest.NewRequest(http.MethodPost, "/auth/otp/request", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusGone {
				t.Fatalf("expected 410 for body %q, got %d body=%s", body, w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), string(apierr.CodeAuthEndpointDisabled)) {
				t.Fatalf("expected CodeAuthEndpointDisabled in body, got %s", w.Body.String())
			}
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

	t.Run("Sessions: ListActiveSessions returns 200 with current session", func(t *testing.T) {
		famID := uuid.New()
		ownerUser := &domain.User{
			ID:           uuid.New(),
			Role:         domain.RoleOwner,
			TokenVersion: 1,
		}
		tok, err := auth.IssueAccessTokenWithFamily(jwtSecret, ownerUser, &famID)
		if err != nil {
			t.Fatalf("IssueAccessTokenWithFamily: %v", err)
		}

		authSvc.sessionsToReturn = []domain.ActiveSession{
			{
				ID:          famID,
				DeviceLabel: "Safari on iPhone",
				IPAddress:   "1.2.3.4",
			},
			{
				ID:          uuid.New(),
				DeviceLabel: "Chrome on Windows",
				IPAddress:   "5.6.7.8",
			},
		}

		req := httptest.NewRequest(http.MethodGet, "/auth/sessions", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for GetActiveSessions, got %d body=%s", w.Code, w.Body.String())
		}

		var resp struct {
			Sessions []domain.ActiveSession `json:"sessions"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if len(resp.Sessions) != 2 {
			t.Fatalf("expected 2 sessions, got %d", len(resp.Sessions))
		}
		if !resp.Sessions[0].IsCurrent {
			t.Errorf("expected session 0 to be marked current")
		}
		if resp.Sessions[1].IsCurrent {
			t.Errorf("expected session 1 not to be marked current")
		}
	})

	t.Run("Sessions: DeleteSession revokes session family and returns 200", func(t *testing.T) {
		famToRevoke := uuid.New()
		ownerUser := &domain.User{
			ID:           uuid.New(),
			Role:         domain.RoleOwner,
			TokenVersion: 1,
		}
		tok, _ := auth.IssueAccessToken(jwtSecret, ownerUser)

		req := httptest.NewRequest(http.MethodDelete, "/auth/sessions/"+famToRevoke.String(), nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for DeleteSession, got %d body=%s", w.Code, w.Body.String())
		}
		if len(authSvc.revokedSessionIDs) == 0 || authSvc.revokedSessionIDs[len(authSvc.revokedSessionIDs)-1] != famToRevoke {
			t.Fatalf("expected revoked session %v, got %v", famToRevoke, authSvc.revokedSessionIDs)
		}
	})

	t.Run("Sessions: DeleteSession with invalid UUID returns 400", func(t *testing.T) {
		ownerUser := &domain.User{
			ID:           uuid.New(),
			Role:         domain.RoleOwner,
			TokenVersion: 1,
		}
		tok, _ := auth.IssueAccessToken(jwtSecret, ownerUser)

		req := httptest.NewRequest(http.MethodDelete, "/auth/sessions/invalid-not-a-uuid", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid session id, got %d body=%s", w.Code, w.Body.String())
		}
	})
}
