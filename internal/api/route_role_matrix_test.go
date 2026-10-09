package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// isPublicRoute determines if a registered route is designed to be public/unauthenticated.
func isPublicRoute(path, method string) bool {
	publicExact := map[string]bool{
		"/healthz":                             true,
		"/metrics":                             true,
		"/api/healthz":                         true,
		"/api/metrics":                         true,
		"/api/locales":                         true,
		"/api/push/vapid-public-key":           true,
		"/api/owner/calendar.ics":              true,
		"/api/auth/otp/request":                true,
		"/api/auth/otp/verify":                 true,
		"/api/auth/firebase":                   true,
		"/api/auth/refresh":                    true,
		"/api/auth/logout":                     true,
		"/api/public/cashfree/kyc/webhook":     true,
		"/webhooks/cashfree":                   true,
		"/webhooks/cashfree/payouts":           true,
		"/webhooks/cashfree/settlements":       true,
	}
	if publicExact[path] {
		return true
	}
	if strings.HasPrefix(path, "/p/") {
		return true
	}
	if strings.HasPrefix(path, "/api/join/invite/") {
		return true
	}
	return false
}

// TestAPI_ComprehensiveRouteRoleMatrix derives every route registered in NewRouter
// and verifies strict RBAC gating across all identity profiles:
// 1. Anonymous (unauthenticated) -> 401 Unauthorized on all protected routes
// 2. Tenant -> 403 Forbidden on /api/owner/* and /api/manager/*
// 3. Manager -> 403 Forbidden on /api/owner/* and /api/tenant/*
// 4. Owner -> 403 Forbidden on /api/tenant/*, passes /api/owner/* and /api/manager/*
// 5. Pending-Join Tenant -> 403 Forbidden on /api/owner/*, /api/manager/*, /api/tenant/*
func TestAPI_ComprehensiveRouteRoleMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "role-matrix-test-secret-32-chars-long!"
	deps := Deps{
		JWTSecret: jwtSecret,
	}
	router := NewRouter(deps)
	routes := router.Routes()

	if len(routes) == 0 {
		t.Fatal("expected registered routes from router, got 0")
	}

	propID := uuid.New()
	tenantID := uuid.New()

	// Mint JWTs for each role
	ownerUser := &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleOwner,
		PropertyID:   &propID,
		TokenVersion: 1,
	}
	ownerToken, err := auth.IssueAccessToken(jwtSecret, ownerUser)
	if err != nil {
		t.Fatalf("mint owner token: %v", err)
	}

	managerUser := &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleManager,
		PropertyID:   &propID,
		TokenVersion: 1,
	}
	managerToken, err := auth.IssueAccessToken(jwtSecret, managerUser)
	if err != nil {
		t.Fatalf("mint manager token: %v", err)
	}

	tenantUser := &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleTenant,
		TenantID:     &tenantID,
		PropertyID:   &propID,
		TokenVersion: 1,
	}
	tenantToken, err := auth.IssueAccessToken(jwtSecret, tenantUser)
	if err != nil {
		t.Fatalf("mint tenant token: %v", err)
	}

	pendingJoinUser := &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleTenant,
		TenantID:     nil, // pending join
		PropertyID:   &propID,
		TokenVersion: 1,
	}
	pendingJoinToken, err := auth.IssueAccessToken(jwtSecret, pendingJoinUser)
	if err != nil {
		t.Fatalf("mint pending join token: %v", err)
	}

	// Helper to build test request path by substituting Gin route parameters
	buildPath := func(ginPath string) string {
		p := ginPath
		p = strings.ReplaceAll(p, ":id", uuid.New().String())
		p = strings.ReplaceAll(p, ":token", "test-token-uuid")
		p = strings.ReplaceAll(p, ":code", "TEST1234")
		p = strings.ReplaceAll(p, ":action", "approve")
		return p
	}

	t.Run("Anonymous_MustRejectProtectedRoutesWith401", func(t *testing.T) {
		testedCount := 0
		for _, rt := range routes {
			if !strings.HasPrefix(rt.Path, "/api") && !strings.HasPrefix(rt.Path, "/webhooks") && !strings.HasPrefix(rt.Path, "/p/") {
				continue
			}
			if isPublicRoute(rt.Path, rt.Method) {
				continue
			}

			testedCount++
			reqPath := buildPath(rt.Path)
			req := httptest.NewRequest(rt.Method, reqPath, nil)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("[%s %s] expected 401 Unauthorized for anonymous, got %d (body: %s)",
					rt.Method, rt.Path, rec.Code, rec.Body.String())
				continue
			}

			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err == nil {
				if code, ok := body["code"].(string); ok {
					if code != string(apierr.CodeAuthMissingToken) {
						t.Errorf("[%s %s] expected code %q, got %q", rt.Method, rt.Path, apierr.CodeAuthMissingToken, code)
					}
				}
			}
		}
		if testedCount < 100 {
			t.Fatalf("expected to test at least 100 protected routes for anonymous rejection, tested %d", testedCount)
		}
		t.Logf("✓ Verified %d protected routes strictly reject anonymous access with 401", testedCount)
	})

	t.Run("Tenant_CannotAccessOwnerOrManagerRoutes", func(t *testing.T) {
		for _, rt := range routes {
			isOwner := strings.HasPrefix(rt.Path, "/api/owner") && rt.Path != "/api/owner/calendar.ics"
			isManager := strings.HasPrefix(rt.Path, "/api/manager")

			if !isOwner && !isManager {
				continue
			}

			reqPath := buildPath(rt.Path)
			req := httptest.NewRequest(rt.Method, reqPath, nil)
			req.Header.Set("Authorization", "Bearer "+tenantToken)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("[%s %s] expected 403 Forbidden for tenant accessing owner/manager route, got %d (body: %s)",
					rt.Method, rt.Path, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("Manager_CannotAccessOwnerOrTenantRoutes", func(t *testing.T) {
		for _, rt := range routes {
			isOwner := strings.HasPrefix(rt.Path, "/api/owner") && rt.Path != "/api/owner/calendar.ics"
			isTenant := strings.HasPrefix(rt.Path, "/api/tenant")

			if !isOwner && !isTenant {
				continue
			}

			reqPath := buildPath(rt.Path)
			req := httptest.NewRequest(rt.Method, reqPath, nil)
			req.Header.Set("Authorization", "Bearer "+managerToken)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("[%s %s] expected 403 Forbidden for manager accessing owner/tenant route, got %d (body: %s)",
					rt.Method, rt.Path, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("Owner_CannotAccessTenantRoutes", func(t *testing.T) {
		for _, rt := range routes {
			if !strings.HasPrefix(rt.Path, "/api/tenant") {
				continue
			}

			reqPath := buildPath(rt.Path)
			req := httptest.NewRequest(rt.Method, reqPath, nil)
			req.Header.Set("Authorization", "Bearer "+ownerToken)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("[%s %s] expected 403 Forbidden for owner accessing tenant route, got %d (body: %s)",
					rt.Method, rt.Path, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("PendingJoin_CannotAccessOwnerOrManagerOrTenantRoutes", func(t *testing.T) {
		for _, rt := range routes {
			isOwner := strings.HasPrefix(rt.Path, "/api/owner") && rt.Path != "/api/owner/calendar.ics"
			isManager := strings.HasPrefix(rt.Path, "/api/manager")
			isTenant := strings.HasPrefix(rt.Path, "/api/tenant")

			if !isOwner && !isManager && !isTenant {
				continue
			}

			reqPath := buildPath(rt.Path)
			req := httptest.NewRequest(rt.Method, reqPath, nil)
			req.Header.Set("Authorization", "Bearer "+pendingJoinToken)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Errorf("[%s %s] expected 403 Forbidden for pending-join user, got %d (body: %s)",
					rt.Method, rt.Path, rec.Code, rec.Body.String())
			}
		}
	})

	t.Run("Owner_AuthorizedOnOwnerAndManagerRoutes", func(t *testing.T) {
		// Verify that owner passes auth middleware on owner and manager routes
		// (i.e. status is NOT 401 Unauthorized or 403 Forbidden from auth layer)
		sampleRoutes := []struct {
			method string
			path   string
		}{
			{http.MethodGet, "/api/owner/properties"},
			{http.MethodGet, "/api/owner/tenants"},
			{http.MethodGet, "/api/owner/dues"},
			{http.MethodGet, "/api/owner/finance/summary"},
			{http.MethodGet, "/api/manager/inspections"},
			{http.MethodGet, "/api/manager/hazards"},
			{http.MethodGet, "/api/notifications"},
			{http.MethodGet, "/api/me/preferences"},
		}

		for _, sr := range sampleRoutes {
			req := httptest.NewRequest(sr.method, sr.path, nil)
			req.Header.Set("Authorization", "Bearer "+ownerToken)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)

			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Errorf("[%s %s] owner should pass auth gate, but got auth rejection code %d (body: %s)",
					sr.method, sr.path, rec.Code, rec.Body.String())
			}
		}
	})
}
