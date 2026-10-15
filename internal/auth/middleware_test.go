package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockTenantRepo struct {
	tenant *domain.Tenant
	err    error
}

func (m *mockTenantRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return m.tenant, m.err
}

func (m *mockTenantRepo) GetByPhone(ctx context.Context, phone string) (*domain.Tenant, error) {
	return nil, nil
}

func TestRequireTenant_VacatedReturns403(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "middleware-test-secret"
	tenantID := uuid.New()
	propID := uuid.New()
	user := &domain.User{
		ID:         uuid.New(),
		Phone:      "9999999999",
		Role:       domain.RoleTenant,
		TenantID:   &tenantID,
		PropertyID: &propID,
	}
	token, err := IssueToken(secret, user)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	repo := &mockTenantRepo{
		tenant: &domain.Tenant{
			ID:     tenantID,
			Status: domain.TenantStatusVacated,
		},
	}

	r := gin.New()
	r.GET("/me", RequireTenant(secret, repo, nil), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "access revoked") {
		t.Fatalf("expected access revoked in body, got %s", w.Body.String())
	}
}

func TestRequireTenant_ActivePasses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "middleware-test-secret"
	tenantID := uuid.New()
	propID := uuid.New()
	user := &domain.User{
		ID:         uuid.New(),
		Phone:      "9999999999",
		Role:       domain.RoleTenant,
		TenantID:   &tenantID,
		PropertyID: &propID,
	}
	token, err := IssueToken(secret, user)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	repo := &mockTenantRepo{
		tenant: &domain.Tenant{
			ID:     tenantID,
			Status: domain.TenantStatusActive,
		},
	}

	r := gin.New()
	r.GET("/me", RequireTenant(secret, repo, nil), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRequireTenant_PendingAllocationPasses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "middleware-test-secret"
	tenantID := uuid.New()
	propID := uuid.New()
	user := &domain.User{
		ID:         uuid.New(),
		Phone:      "9999999999",
		Role:       domain.RoleTenant,
		TenantID:   &tenantID,
		PropertyID: &propID,
	}
	token, err := IssueToken(secret, user)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	repo := &mockTenantRepo{
		tenant: &domain.Tenant{ID: tenantID, Status: domain.TenantStatusPendingAllocation},
	}
	r := gin.New()
	r.GET("/me", RequireTenant(secret, repo, nil), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
}

func TestRequireTenant_PendingJoinForbidden(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "middleware-test-secret"
	propID := uuid.New()
	user := &domain.User{
		ID:         uuid.New(),
		Phone:      "9999999999",
		Role:       domain.RoleTenant,
		PropertyID: &propID,
	}
	token, err := IssueToken(secret, user)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	r := gin.New()
	r.GET("/me", RequireTenant(secret, &mockTenantRepo{}, nil), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	req := httptest.NewRequest(http.MethodGet, "/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "complete your profile") {
		t.Fatalf("body=%s", w.Body.String())
	}
}

type mockUserLookup struct {
	user *domain.User
	err  error
}

func (m *mockUserLookup) GetByID(context.Context, uuid.UUID) (*domain.User, error) {
	return m.user, m.err
}

func TestRequireOwner_StaleTokenVersionReturns403(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "middleware-test-secret"
	propID := uuid.New()
	user := &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleOwner,
		PropertyID:   &propID,
		TokenVersion: 1,
	}
	token, err := IssueToken(secret, user)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	live := *user
	live.TokenVersion = 2
	r := gin.New()
	r.GET("/owner", RequireOwner(secret, &mockUserLookup{user: &live}), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	req := httptest.NewRequest(http.MethodGet, "/owner", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "access revoked") {
		t.Fatalf("body=%s", w.Body.String())
	}
}

func TestRequireOwner_MatchingTokenVersionPasses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "middleware-test-secret"
	propID := uuid.New()
	user := &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleOwner,
		PropertyID:   &propID,
		TokenVersion: 1,
	}
	token, err := IssueToken(secret, user)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	r := gin.New()
	r.GET("/owner", RequireOwner(secret, &mockUserLookup{user: user}), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	req := httptest.NewRequest(http.MethodGet, "/owner", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestMiddlewareGuards_AllSixCodes explicitly asserts HTTP status, error message,
// and machine-readable code for all six guard branches in auth middleware.
func TestMiddlewareGuards_AllSixCodes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "guard-test-secret"
	propID := uuid.New()
	tenantID := uuid.New()

	assertEnvelope := func(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantErr string, wantCode apierr.Code) {
		t.Helper()
		if w.Code != wantStatus {
			t.Errorf("status = %d, want %d", w.Code, wantStatus)
		}
		var env apierr.ErrorEnvelope
		if err := json.NewDecoder(w.Body).Decode(&env); err != nil {
			t.Fatalf("failed to decode response %q: %v", w.Body.String(), err)
		}
		if env.Error != wantErr {
			t.Errorf("error = %q, want %q", env.Error, wantErr)
		}
		if env.Code != wantCode {
			t.Errorf("code = %q, want %q", env.Code, wantCode)
		}
	}

	// 1. auth.missingToken (401)
	t.Run("auth.missingToken", func(t *testing.T) {
		r := gin.New()
		r.GET("/guarded", RequireOwner(secret, nil), func(c *gin.Context) {})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/guarded", nil)
		r.ServeHTTP(w, req)
		assertEnvelope(t, w, http.StatusUnauthorized, "missing bearer token", apierr.CodeAuthMissingToken)
	})

	// 2. auth.invalidToken (401)
	t.Run("auth.invalidToken", func(t *testing.T) {
		r := gin.New()
		r.GET("/guarded", RequireOwner(secret, nil), func(c *gin.Context) {})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/guarded", nil)
		req.Header.Set("Authorization", "Bearer invalid.jwt.token")
		r.ServeHTTP(w, req)
		assertEnvelope(t, w, http.StatusUnauthorized, "invalid token", apierr.CodeAuthInvalidToken)
	})

	// 3. auth.accessRevoked (403)
	t.Run("auth.accessRevoked", func(t *testing.T) {
		user := &domain.User{
			ID:         uuid.New(),
			Role:       domain.RoleTenant,
			TenantID:   &tenantID,
			PropertyID: &propID,
		}
		token, _ := IssueToken(secret, user)
		repo := &mockTenantRepo{
			tenant: &domain.Tenant{ID: tenantID, Status: domain.TenantStatusVacated},
		}
		r := gin.New()
		r.GET("/guarded", RequireTenant(secret, repo, nil), func(c *gin.Context) {})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/guarded", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		r.ServeHTTP(w, req)
		assertEnvelope(t, w, http.StatusForbidden, "access revoked", apierr.CodeAuthAccessRevoked)
	})

	// 4. auth.forbidden (403)
	t.Run("auth.forbidden", func(t *testing.T) {
		tenantUser := &domain.User{
			ID:         uuid.New(),
			Role:       domain.RoleTenant,
			PropertyID: &propID,
		}
		token, _ := IssueToken(secret, tenantUser)
		r := gin.New()
		r.GET("/guarded", RequireOwner(secret, nil), func(c *gin.Context) {})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/guarded", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		r.ServeHTTP(w, req)
		assertEnvelope(t, w, http.StatusForbidden, "forbidden", apierr.CodeAuthForbidden)
	})

	// 5. auth.profileIncomplete (403)
	t.Run("auth.profileIncomplete", func(t *testing.T) {
		incompleteUser := &domain.User{
			ID:         uuid.New(),
			Role:       domain.RoleTenant,
			PropertyID: &propID,
			TenantID:   nil, // incomplete profile
		}
		token, _ := IssueToken(secret, incompleteUser)
		r := gin.New()
		r.GET("/guarded", RequireTenant(secret, &mockTenantRepo{}, nil), func(c *gin.Context) {})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/guarded", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		r.ServeHTTP(w, req)
		assertEnvelope(t, w, http.StatusForbidden, "complete your profile to continue", apierr.CodeAuthProfileIncomplete)
	})

	// 6. auth.alreadyActivated (403)
	t.Run("auth.alreadyActivated", func(t *testing.T) {
		activeUser := &domain.User{
			ID:         uuid.New(),
			Role:       domain.RoleTenant,
			TenantID:   &tenantID, // already has tenant row
			PropertyID: &propID,
		}
		token, _ := IssueToken(secret, activeUser)
		r := gin.New()
		r.GET("/guarded", RequirePendingJoin(secret, nil), func(c *gin.Context) {})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/guarded", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		r.ServeHTTP(w, req)
		assertEnvelope(t, w, http.StatusForbidden, "already activated", apierr.CodeAuthAlreadyActivated)
	})
}

type countingUserLookup struct {
	user  *domain.User
	calls int
}

func (c *countingUserLookup) GetByID(context.Context, uuid.UUID) (*domain.User, error) {
	c.calls++
	return c.user, nil
}

func TestTokenVersionCache_HitAvoidsDBRoundTrip(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ResetTokenVersionCache()
	defer ResetTokenVersionCache()

	const secret = "middleware-test-secret"
	propID := uuid.New()
	user := &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleOwner,
		PropertyID:   &propID,
		TokenVersion: 1,
	}
	token, err := IssueToken(secret, user)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	lookup := &countingUserLookup{user: user}
	r := gin.New()
	r.GET("/owner", RequireOwner(secret, lookup), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// Request 1: Cache miss -> lookup called
	req1 := httptest.NewRequest(http.MethodGet, "/owner", nil)
	req1.Header.Set("Authorization", "Bearer "+token)
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("req1: expected 200, got %d", w1.Code)
	}
	if lookup.calls != 1 {
		t.Fatalf("req1: expected 1 lookup call, got %d", lookup.calls)
	}

	// Request 2: Cache hit -> lookup NOT called again
	req2 := httptest.NewRequest(http.MethodGet, "/owner", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("req2: expected 200, got %d", w2.Code)
	}
	if lookup.calls != 1 {
		t.Fatalf("req2: expected lookup call count to remain 1 (cache hit), got %d", lookup.calls)
	}

	// Invalidate cache for user
	InvalidateTokenVersionCache(user.ID)

	// Request 3: Cache miss after invalidation -> lookup called
	req3 := httptest.NewRequest(http.MethodGet, "/owner", nil)
	req3.Header.Set("Authorization", "Bearer "+token)
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusOK {
		t.Fatalf("req3: expected 200, got %d", w3.Code)
	}
	if lookup.calls != 2 {
		t.Fatalf("req3: expected 2 lookup calls after invalidation, got %d", lookup.calls)
	}
}

func TestTokenVersionCache_RevocationAfterInvalidation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ResetTokenVersionCache()
	defer ResetTokenVersionCache()

	const secret = "middleware-test-secret"
	propID := uuid.New()
	user := &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleOwner,
		PropertyID:   &propID,
		TokenVersion: 1,
	}
	token, err := IssueToken(secret, user)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}

	lookupUser := *user
	lookup := &countingUserLookup{user: &lookupUser}
	r := gin.New()
	r.GET("/owner", RequireOwner(secret, lookup), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// Request 1: Valid
	req1 := httptest.NewRequest(http.MethodGet, "/owner", nil)
	req1.Header.Set("Authorization", "Bearer "+token)
	w1 := httptest.NewRecorder()
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("req1: expected 200, got %d", w1.Code)
	}

	// Bump token version on the user
	lookupUser.TokenVersion = 2

	// Request 2 without invalidation: still passes due to cache
	req2 := httptest.NewRequest(http.MethodGet, "/owner", nil)
	req2.Header.Set("Authorization", "Bearer "+token)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("req2: expected 200 from cache, got %d", w2.Code)
	}

	// Invalidate cache explicitly (as done on revoke)
	InvalidateTokenVersionCache(user.ID)

	// Request 3 after invalidation: 403 Forbidden
	req3 := httptest.NewRequest(http.MethodGet, "/owner", nil)
	req3.Header.Set("Authorization", "Bearer "+token)
	w3 := httptest.NewRecorder()
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusForbidden {
		t.Fatalf("req3: expected 403 after invalidation, got %d", w3.Code)
	}
}

