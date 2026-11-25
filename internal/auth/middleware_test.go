package auth

import (
	"context"
	"encoding/json"
	"errors"
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
	token, err := IssueAccessToken(secret, user)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
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
	token, err := IssueAccessToken(secret, user)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
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
	token, err := IssueAccessToken(secret, user)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
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
	token, err := IssueAccessToken(secret, user)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
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
	token, err := IssueAccessToken(secret, user)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
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
	token, err := IssueAccessToken(secret, user)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
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
		token, _ := IssueAccessToken(secret, user)
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
		token, _ := IssueAccessToken(secret, tenantUser)
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
		token, _ := IssueAccessToken(secret, incompleteUser)
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
		token, _ := IssueAccessToken(secret, activeUser)
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
	token, err := IssueAccessToken(secret, user)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
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
	token, err := IssueAccessToken(secret, user)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
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

func TestFinancialMutations_BypassTokenVersionCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ResetTokenVersionCache()
	defer ResetTokenVersionCache()

	const secret = "middleware-financial-test-secret"
	propID := uuid.New()
	user := &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleOwner,
		PropertyID:   &propID,
		TokenVersion: 1,
	}
	token, err := IssueAccessToken(secret, user)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	lookupUser := *user
	lookup := &countingUserLookup{user: &lookupUser}
	r := gin.New()
	r.POST("/api/owner/finance/expenses", RequireOwner(secret, lookup), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	r.GET("/api/owner/dashboard", RequireOwner(secret, lookup), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})

	// 1. GET /api/owner/dashboard populates the cache
	reqGet := httptest.NewRequest(http.MethodGet, "/api/owner/dashboard", nil)
	reqGet.Header.Set("Authorization", "Bearer "+token)
	wGet := httptest.NewRecorder()
	r.ServeHTTP(wGet, reqGet)
	if wGet.Code != http.StatusOK || lookup.calls != 1 {
		t.Fatalf("expected GET 200 with 1 lookup call, got code %d calls %d", wGet.Code, lookup.calls)
	}

	// 2. Token version bumped in DB (simulating revocation on another instance without local cache eviction)
	lookupUser.TokenVersion = 2

	// 3. GET /api/owner/dashboard still succeeds because read route uses cache
	reqGet2 := httptest.NewRequest(http.MethodGet, "/api/owner/dashboard", nil)
	reqGet2.Header.Set("Authorization", "Bearer "+token)
	wGet2 := httptest.NewRecorder()
	r.ServeHTTP(wGet2, reqGet2)
	if wGet2.Code != http.StatusOK || lookup.calls != 1 {
		t.Fatalf("expected GET 200 from cache with 1 lookup call, got code %d calls %d", wGet2.Code, lookup.calls)
	}

	// 4. POST /api/owner/finance/expenses MUST bypass cache and immediately reject with 403
	reqPost := httptest.NewRequest(http.MethodPost, "/api/owner/finance/expenses", nil)
	reqPost.Header.Set("Authorization", "Bearer "+token)
	wPost := httptest.NewRecorder()
	r.ServeHTTP(wPost, reqPost)
	if wPost.Code != http.StatusForbidden {
		t.Fatalf("expected financial mutation to return 403 Forbidden on revoked token version, got %d", wPost.Code)
	}
	if lookup.calls != 2 {
		t.Fatalf("expected lookup call count to increment to 2 (bypassing cache), got %d", lookup.calls)
	}
}

type mockFamilyUserLookup struct {
	user         *domain.User
	familyActive bool
	familyErr    error
}

func (m *mockFamilyUserLookup) GetByID(context.Context, uuid.UUID) (*domain.User, error) {
	return m.user, nil
}

func (m *mockFamilyUserLookup) IsFamilyActive(context.Context, uuid.UUID) (bool, error) {
	return m.familyActive, m.familyErr
}

func TestRequireOwner_FamilyBoundSessionRevocation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "middleware-family-test-secret"
	propID := uuid.New()
	familyID := uuid.New()
	user := &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleOwner,
		PropertyID:   &propID,
		TokenVersion: 1,
	}

	token, err := IssueAccessTokenWithFamily(secret, user, &familyID)
	if err != nil {
		t.Fatalf("IssueAccessTokenWithFamily: %v", err)
	}

	// 1. Active family in durable lookup passes
	{
		ResetRevokedFamilyCache()
		lookup := &mockFamilyUserLookup{user: user, familyActive: true}
		r := gin.New()
		r.GET("/owner", RequireOwner(secret, lookup), func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})
		req := httptest.NewRequest(http.MethodGet, "/owner", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK for active family, got %d", w.Code)
		}
	}

	// 2. Inactive/revoked family in durable lookup returns 403 Forbidden
	{
		ResetRevokedFamilyCache()
		lookup := &mockFamilyUserLookup{user: user, familyActive: false}
		r := gin.New()
		r.GET("/owner", RequireOwner(secret, lookup), func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})
		req := httptest.NewRequest(http.MethodGet, "/owner", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for revoked family, got %d", w.Code)
		}
	}

	// 3. Database error during durable lookup returns 503 Service Unavailable
	{
		ResetRevokedFamilyCache()
		lookup := &mockFamilyUserLookup{user: user, familyErr: errors.New("database connection down")}
		r := gin.New()
		r.GET("/owner", RequireOwner(secret, lookup), func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})
		req := httptest.NewRequest(http.MethodGet, "/owner", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 Service Unavailable on lookup error, got %d", w.Code)
		}
	}

	// 4. Missing SessionFamilyChecker on family-bound token fails closed with 403 Forbidden
	{
		ResetRevokedFamilyCache()
		plainLookup := &mockUserLookup{user: user}
		r := gin.New()
		r.GET("/owner", RequireOwner(secret, plainLookup), func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})
		req := httptest.NewRequest(http.MethodGet, "/owner", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden when family lookup is unconfigured, got %d", w.Code)
		}
	}
}

func TestRequirePlatformOps(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "platform-ops-test-secret"

	t.Run("fails when token is missing", func(t *testing.T) {
		r := gin.New()
		r.GET("/platform/payouts", RequirePlatformOps(secret, nil), func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/platform/payouts", nil)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", w.Code)
		}
	})

	t.Run("fails when role is owner", func(t *testing.T) {
		owner := &domain.User{ID: uuid.New(), Role: domain.RoleOwner, Phone: "+919999900001"}
		token, _ := IssueAccessToken(secret, owner)
		r := gin.New()
		r.GET("/platform/payouts", RequirePlatformOps(secret, nil), func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/platform/payouts", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403, got %d", w.Code)
		}
	})

	t.Run("passes when role is platform_ops", func(t *testing.T) {
		opsUser := &domain.User{ID: uuid.New(), Role: domain.RolePlatformOps, Phone: "+919999900002"}
		token, _ := IssueAccessToken(secret, opsUser)
		r := gin.New()
		r.GET("/platform/payouts", RequirePlatformOps(secret, nil), func(c *gin.Context) {
			c.JSON(http.StatusOK, gin.H{"ok": true})
		})
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/platform/payouts", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d", w.Code)
		}
	})
}

func TestAuthoritativeMutations_BypassTokenVersionCache(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "middleware-authoritative-test-secret"
	ResetTokenVersionCache()
	defer ResetTokenVersionCache()

	propID := uuid.New()
	user := &domain.User{
		ID:           uuid.New(),
		Phone:        "+919999900010",
		Role:         domain.RoleOwner,
		PropertyID:   &propID,
		TokenVersion: 1,
	}
	token, err := IssueAccessToken(secret, user)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}

	// Prime the in-memory token version cache with version 1, then bump DB to version 2.
	setCachedTokenVersion(user.ID, 1)
	users := &mockUserLookup{
		user: &domain.User{
			ID:           user.ID,
			Phone:        user.Phone,
			Role:         domain.RoleOwner,
			PropertyID:   &propID,
			TokenVersion: 2,
		},
	}

	r := gin.New()
	authMW := RequireOwner(secret, users)
	r.POST("/api/owner/kyc/:id/approve", authMW, func(c *gin.Context) { c.Status(http.StatusOK) })
	r.POST("/api/owner/join-requests/:id/activate", authMW, func(c *gin.Context) { c.Status(http.StatusOK) })
	r.PUT("/api/owner/managers/:id/capabilities", authMW, func(c *gin.Context) { c.Status(http.StatusOK) })
	r.POST("/api/owner/slot-invites", authMW, func(c *gin.Context) { c.Status(http.StatusOK) })

	cases := []struct {
		method string
		path   string
	}{
		{http.MethodPost, "/api/owner/kyc/" + uuid.NewString() + "/approve"},
		{http.MethodPost, "/api/owner/join-requests/" + uuid.NewString() + "/activate"},
		{http.MethodPut, "/api/owner/managers/" + uuid.NewString() + "/capabilities"},
		{http.MethodPost, "/api/owner/slot-invites"},
	}

	for _, tc := range cases {
		setCachedTokenVersion(user.ID, 1) // re-prime stale cache for each path
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusForbidden {
			t.Fatalf("%s %s: expected 403 Forbidden on revoked token version despite stale cache, got %d", tc.method, tc.path, w.Code)
		}
	}
}
