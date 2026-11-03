package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type stubSessionStore struct {
	user *domain.User
}

func (s *stubSessionStore) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	if s.user == nil || s.user.ID != id {
		return nil, errSessionNotFound
	}
	cp := *s.user
	return &cp, nil
}

func (s *stubSessionStore) IncrementTokenVersion(_ context.Context, id uuid.UUID) error {
	if s.user == nil || s.user.ID != id {
		return errSessionNotFound
	}
	if s.user.TokenVersion < 1 {
		s.user.TokenVersion = 1
	}
	s.user.TokenVersion++
	return nil
}

var errSessionNotFound = errString("user not found")

type errString string

func (e errString) Error() string { return string(e) }

type revokeOthersAuthService struct {
	stubPayoutAuthService
	called bool
}

func (s *revokeOthersAuthService) RevokeOtherSessions(context.Context, uuid.UUID, *uuid.UUID, string) error {
	s.called = true
	return nil
}

func TestRevokeOtherSessionsPreservesCallerJWT(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test-secret-key-with-sufficient-length-32"
	propID := uuid.New()
	user := &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleOwner,
		PropertyID:   &propID,
		TokenVersion: 1,
	}
	token, err := auth.IssueToken(jwtSecret, user)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	store := &stubSessionStore{user: user}
	svc := &revokeOthersAuthService{}
	r := NewRouter(Deps{
		JWTSecret:    jwtSecret,
		AuthUserRepo: store,
		Auth:         svc,
	})
	r.GET("/session-check", auth.RequireOwner(jwtSecret, store), func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodPost, "/api/auth/revoke-sessions", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("revoke status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("json: %v", err)
	}
	if body["ok"] != true {
		t.Fatalf("body=%v", body)
	}

	if !svc.called {
		t.Fatal("other-session revocation was not called")
	}
	req2 := httptest.NewRequest(http.MethodGet, "/session-check", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("caller session must survive revoke-others, got %d body=%s", w2.Code, w2.Body.String())
	}
}
