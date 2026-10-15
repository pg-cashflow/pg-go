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
	joinsvc "github.com/pg-cashflow/pg-go/internal/join"
)

type joinProfileUserStore struct {
	user *domain.User
}

func (s *joinProfileUserStore) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	if s.user == nil || s.user.ID != id {
		return nil, errSessionNotFound
	}
	cp := *s.user
	return &cp, nil
}

func (s *joinProfileUserStore) IncrementTokenVersion(_ context.Context, id uuid.UUID) error {
	if s.user == nil || s.user.ID != id {
		return errSessionNotFound
	}
	if s.user.TokenVersion < 1 {
		s.user.TokenVersion = 1
	}
	return nil
}

type joinProfileJoinStub struct {
	outcomeJoinStub
	tenantID   uuid.UUID
	propertyID uuid.UUID
	userStore  *joinProfileUserStore
}

func (j *joinProfileJoinStub) CompleteOnboarding(_ context.Context, userID uuid.UUID, in joinsvc.ProfileInput) (*domain.JoinRequest, *domain.Tenant, error) {
	jr := &domain.JoinRequest{
		ID:         uuid.New(),
		UserID:     userID,
		PropertyID: j.propertyID,
		Status:     domain.JoinApproved,
		Name:       in.Name,
	}
	t := &domain.Tenant{
		ID:         j.tenantID,
		PropertyID: j.propertyID,
		Status:     domain.TenantStatusPendingAllocation,
		Name:       in.Name,
	}
	j.userStore.user.TenantID = &j.tenantID
	return jr, t, nil
}

type joinSessionAuthStub struct {
	stubAuthService
	jwtSecret string
}

func (s *joinSessionAuthStub) IssueSession(_ context.Context, user *domain.User) (string, string, error) {
	tok, err := auth.IssueToken(s.jwtSecret, user)
	if err != nil {
		return "", "", err
	}
	return tok, "join-test-refresh", nil
}

type joinSessionTenantRepo struct {
	tenant *domain.Tenant
}

func (r *joinSessionTenantRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if r.tenant == nil || r.tenant.ID != id {
		return nil, pgx.ErrNoRows
	}
	cp := *r.tenant
	return &cp, nil
}

func (r *joinSessionTenantRepo) GetByPhone(context.Context, string) (*domain.Tenant, error) {
	return nil, pgx.ErrNoRows
}

func TestJoinProfileIssuesSessionAndTenantRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test-secret-key-with-sufficient-length-32"
	userID := uuid.New()
	tenantID := uuid.New()
	propID := uuid.New()
	user := &domain.User{
		ID:           userID,
		Role:         domain.RoleTenant,
		PropertyID:   &propID,
		Phone:        "+919876543210",
		TokenVersion: 1,
	}
	userStore := &joinProfileUserStore{user: user}
	tenant := &domain.Tenant{
		ID:         tenantID,
		PropertyID: propID,
		Status:     domain.TenantStatusPendingAllocation,
		Name:       "Test Tenant",
	}
	joinStub := &joinProfileJoinStub{
		tenantID:   tenantID,
		propertyID: propID,
		userStore:  userStore,
	}
	r := NewRouter(Deps{
		JWTSecret:      jwtSecret,
		Auth:           &joinSessionAuthStub{jwtSecret: jwtSecret},
		AuthUserRepo:   userStore,
		AuthTenantRepo: &joinSessionTenantRepo{tenant: tenant},
		Joins:          joinStub,
		DueStore:       &stubDueStore{},
	})

	pendingTok, err := auth.IssueToken(jwtSecret, user)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	body, _ := json.Marshal(map[string]any{
		"name":              "Test Tenant",
		"permanent_address": "Addr",
		"consent":           true,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/join", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+pendingTok)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("join status=%d body=%s", w.Code, w.Body.String())
	}

	var joinResp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &joinResp); err != nil {
		t.Fatalf("json: %v", err)
	}
	if joinResp["token"] == nil || joinResp["token"] == "" {
		t.Fatalf("expected token in join response, body=%v", joinResp)
	}
	if joinResp["message"] != "Profile saved." {
		t.Fatalf("message=%v", joinResp["message"])
	}
	userObj, ok := joinResp["user"].(map[string]any)
	if !ok {
		t.Fatalf("user field: %v", joinResp["user"])
	}
	if userObj["tenant_id"] == nil {
		t.Fatalf("expected user.tenant_id set, user=%v", userObj)
	}

	cookies := w.Result().Cookies()
	var refreshCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == RefreshCookieName {
			refreshCookie = c
			break
		}
	}
	if refreshCookie == nil || refreshCookie.Value == "" {
		t.Fatalf("expected pg_refresh cookie, cookies=%v", cookies)
	}

	accTok, _ := joinResp["token"].(string)
	meReq := httptest.NewRequest(http.MethodGet, "/api/tenant/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+accTok)
	meW := httptest.NewRecorder()
	r.ServeHTTP(meW, meReq)
	if meW.Code != http.StatusOK {
		t.Fatalf("GET /tenant/me status=%d body=%s", meW.Code, meW.Body.String())
	}
}
