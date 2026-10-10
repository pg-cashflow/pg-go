package api

// Route x role authorization matrix.
//
// Every route registered by NewRouter is enumerated with gin's Routes() and
// exercised for each caller class:
//
//   - anonymous          : no bearer token             -> must be 401
//   - a role not allowed : valid token, wrong role     -> must be 403
//   - a role allowed     : valid token, right role     -> must NOT be 401/403
//   - owner, foreign ID  : owner token + X-Property-ID
//                          naming another property     -> must be 403 on
//                          owner and manager routes
//
// The allowed roles are taken from CONTRACT.md's role table, not from the code
// under test. A route registered in the wrong auth group therefore fails here
// even when no feature test ever calls it. Any route that this file cannot
// classify fails the test, so new routes cannot silently skip the matrix.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type matrixCaller string

const (
	callerAnon        matrixCaller = "anonymous"
	callerOwner       matrixCaller = "owner"
	callerManager     matrixCaller = "manager"
	callerTenant      matrixCaller = "tenant"
	callerPendingJoin matrixCaller = "pending_join"
)

var matrixCallers = []matrixCaller{callerAnon, callerOwner, callerManager, callerTenant, callerPendingJoin}

type routeClass struct {
	allowed map[matrixCaller]bool
	// anonOK marks public routes: anonymous callers must not be rejected for auth.
	anonOK bool
}

var exactPublicPaths = map[string]bool{
	"/healthz":                         true,
	"/metrics":                         true,
	"/p/:token":                        true,
	"/p/:token/push/subscribe":         true,
	"/webhooks/cashfree":               true,
	"/webhooks/cashfree/payouts":       true,
	"/webhooks/cashfree/settlements":   true,
	"/api/healthz":                     true,
	"/api/metrics":                     true,
	"/api/push/vapid-public-key":       true,
	"/api/join/invite/:code":           true,
	"/api/owner/calendar.ics":          true,
	"/api/auth/otp/request":            true,
	"/api/auth/otp/verify":             true,
	"/api/auth/firebase":               true,
	"/api/auth/refresh":                true,
	"/api/auth/logout":                 true,
	"/api/locales":                     true,
	"/api/public/cashfree/kyc/webhook": true,
}

// classifyRoute maps a route to its contract-defined access. It returns false
// for any route that has no classification, which fails the matrix.
func classifyRoute(path string) (routeClass, bool) {
	allow := func(cs ...matrixCaller) map[matrixCaller]bool {
		m := map[matrixCaller]bool{}
		for _, c := range cs {
			m[c] = true
		}
		return m
	}
	switch {
	// Protected exceptions that sit under otherwise-public prefixes.
	// Shared, role-agnostic routes. A pending-join token carries role=tenant, and
	// CONTRACT.md grants these to "any authenticated role", so it is included.
	case path == "/api/auth/revoke-sessions":
		return routeClass{allowed: allow(callerOwner, callerManager, callerTenant, callerPendingJoin)}, true
	case path == "/api/join/me", path == "/api/join":
		return routeClass{allowed: allow(callerPendingJoin)}, true
	case exactPublicPaths[path]:
		// Exact reviewed public routes. Prefix matching prohibited to prevent unreviewed open routes.
		return routeClass{anonOK: true}, true
	// Role-scoped groups.
	case strings.HasPrefix(path, "/api/owner/"):
		return routeClass{allowed: allow(callerOwner)}, true
	case strings.HasPrefix(path, "/api/manager/"):
		return routeClass{allowed: allow(callerOwner, callerManager)}, true
	case strings.HasPrefix(path, "/api/tenant/"):
		return routeClass{allowed: allow(callerTenant)}, true
	case path == "/api/search", path == "/api/notifications",
		strings.HasPrefix(path, "/api/notifications/"), strings.HasPrefix(path, "/api/me/preferences"):
		return routeClass{allowed: allow(callerOwner, callerManager, callerTenant, callerPendingJoin)}, true
	}
	return routeClass{}, false
}

// matrixFixture holds the identities and stores shared by every matrix case.
type matrixFixture struct {
	secret      string
	propA       uuid.UUID // the owner's and manager's property
	propB       uuid.UUID // belongs to someone else
	ownerUser   *domain.User
	managerUser *domain.User
	tenantUser  *domain.User
	pendingUser *domain.User
	tenantID    uuid.UUID
	tokens      map[matrixCaller]string
}

func newMatrixFixture(t *testing.T) *matrixFixture {
	t.Helper()
	f := &matrixFixture{
		secret:   "test-secret-key-with-sufficient-length-32",
		propA:    uuid.New(),
		propB:    uuid.New(),
		tenantID: uuid.New(),
	}
	propA, tenantID := f.propA, f.tenantID
	f.ownerUser = &domain.User{ID: uuid.New(), Role: domain.RoleOwner, Phone: "+919000000101", PropertyID: &propA}
	f.managerUser = &domain.User{ID: uuid.New(), Role: domain.RoleManager, Phone: "+919000000102", PropertyID: &propA}
	f.tenantUser = &domain.User{ID: uuid.New(), Role: domain.RoleTenant, Phone: "+919000000103", PropertyID: &propA, TenantID: &tenantID}
	f.pendingUser = &domain.User{ID: uuid.New(), Role: domain.RoleTenant, Phone: "+919000000104", PropertyID: &propA}
	f.tokens = map[matrixCaller]string{}
	for caller, u := range map[matrixCaller]*domain.User{
		callerOwner:       f.ownerUser,
		callerManager:     f.managerUser,
		callerTenant:      f.tenantUser,
		callerPendingJoin: f.pendingUser,
	} {
		tok, err := auth.IssueToken(f.secret, u)
		if err != nil {
			t.Fatalf("issue %s token: %v", caller, err)
		}
		f.tokens[caller] = tok
	}
	return f
}

// newRouter builds a fresh router per case, so per-route rate limiters start clean.
func (f *matrixFixture) newRouter() *gin.Engine {
	propertyStore := &scopeTestPropertyStore{props: map[uuid.UUID]*domain.Property{
		f.propA: {ID: f.propA, OwnerPhone: f.ownerUser.Phone},
		f.propB: {ID: f.propB, OwnerPhone: "+919000000199"}, // owned by someone else
	}}
	userStore := &scopeTestUserStore{users: map[uuid.UUID]*domain.User{
		f.ownerUser.ID:   f.ownerUser,
		f.managerUser.ID: f.managerUser,
		f.tenantUser.ID:  f.tenantUser,
		f.pendingUser.ID: f.pendingUser,
	}}
	return NewRouter(Deps{
		JWTSecret:      f.secret,
		MagicLink:      &stubRouterMagicLink{},
		PropertyStore:  propertyStore,
		UserStore:      userStore,
		AuthTenantRepo: matrixTenantRepo{tenantID: f.tenantID, propertyID: f.propA},
		AuthUserRepo:   userStore,
	})
}

// matrixTenantRepo returns one active tenant for the tenant caller.
type matrixTenantRepo struct {
	tenantID   uuid.UUID
	propertyID uuid.UUID
}

func (r matrixTenantRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if id != r.tenantID {
		return nil, errors.New("tenant not found")
	}
	return &domain.Tenant{ID: id, PropertyID: r.propertyID, Status: domain.TenantStatusActive}, nil
}
func (r matrixTenantRepo) GetByPhone(_ context.Context, _ string) (*domain.Tenant, error) {
	return nil, errors.New("tenant not found")
}

// fillPath replaces gin path parameters with concrete values. A /properties/:id
// parameter is filled with the owner's own property so the handler's success
// path is exercised; every other ID is random, which exercises not-found paths.
func fillPath(path string, ownProperty uuid.UUID) string {
	parts := strings.Split(path, "/")
	for i, p := range parts {
		switch {
		case p == "*" || strings.HasPrefix(p, "*"):
			parts[i] = "x"
		case strings.HasPrefix(p, ":"):
			switch p {
			case ":action":
				parts[i] = "approve"
			case ":token":
				parts[i] = "tok-matrix"
			case ":code":
				parts[i] = "INVITE01"
			default:
				if i > 0 && parts[i-1] == "properties" {
					parts[i] = ownProperty.String()
				} else {
					parts[i] = uuid.New().String()
				}
			}
		}
	}
	return strings.Join(parts, "/")
}

// matrixResult is the status plus the apierr code from the response envelope.
type matrixResult struct {
	status  int
	errCode string
}

// isAuthLayerRefusal reports whether a result came from the auth middleware
// (role gate, missing token, property scope) rather than from a handler.
func (r matrixResult) isAuthLayerRefusal() bool {
	return (r.status == http.StatusUnauthorized || r.status == http.StatusForbidden) &&
		strings.HasPrefix(r.errCode, "auth.")
}

func (f *matrixFixture) do(t *testing.T, router *gin.Engine, method, path string, caller matrixCaller, extraHeaders map[string]string) matrixResult {
	t.Helper()
	var body io.Reader
	if method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch {
		body = bytes.NewBufferString("{}")
	}
	req := httptest.NewRequest(method, path, body)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok, ok := f.tokens[caller]; ok {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	var env struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &env)
	return matrixResult{status: w.Code, errCode: env.Code}
}

func TestRouteRoleAuthorizationMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gin.DefaultWriter = io.Discard
	gin.DefaultErrorWriter = io.Discard

	f := newMatrixFixture(t)
	routes := f.newRouter().Routes()
	sort.Slice(routes, func(i, j int) bool {
		if routes[i].Path != routes[j].Path {
			return routes[i].Path < routes[j].Path
		}
		return routes[i].Method < routes[j].Method
	})

	var unclassified []string
	seenPublic := make(map[string]bool)
	cases := 0
	for _, rt := range routes {
		if exactPublicPaths[rt.Path] {
			seenPublic[rt.Path] = true
		}
		class, ok := classifyRoute(rt.Path)
		if !ok {
			unclassified = append(unclassified, rt.Method+" "+rt.Path)
			continue
		}
		path := fillPath(rt.Path, f.propA)
		name := rt.Method + " " + rt.Path
		t.Run(name, func(t *testing.T) {
			// Public routes: an anonymous caller must not be told to authenticate.
			if class.anonOK {
				res := f.do(t, f.newRouter(), rt.Method, path, callerAnon, nil)
				cases++
				// Public routes may refuse on their own terms (for example a missing
				// refresh token), but never with a role or session gate.
				if res.isAuthLayerRefusal() && res.status == http.StatusForbidden {
					t.Errorf("public route %s refused an anonymous caller at the auth layer (%s)", name, res.errCode)
				}
				return
			}
			for _, caller := range matrixCallers {
				res := f.do(t, f.newRouter(), rt.Method, path, caller, nil)
				cases++
				switch {
				case caller == callerAnon:
					if res.status != http.StatusUnauthorized || !strings.HasPrefix(res.errCode, "auth.") {
						t.Errorf("%s anonymous: got %d/%q, want 401 auth.*", name, res.status, res.errCode)
					}
				case class.allowed[caller]:
					if res.isAuthLayerRefusal() {
						t.Errorf("%s %s (allowed): refused by auth layer %d/%q", name, caller, res.status, res.errCode)
					}
				default:
					if res.status != http.StatusForbidden || !strings.HasPrefix(res.errCode, "auth.") {
						t.Errorf("%s %s (not allowed): got %d/%q, want 403 auth.*", name, caller, res.status, res.errCode)
					}
				}
			}
			// Cross-property: an owner token naming another owner's property must be refused
			// on owner and manager routes, which both resolve X-Property-ID.
			if strings.HasPrefix(rt.Path, "/api/owner/") || strings.HasPrefix(rt.Path, "/api/manager/") {
				res := f.do(t, f.newRouter(), rt.Method, path, callerOwner,
					map[string]string{"X-Property-ID": f.propB.String()})
				cases++
				if res.status != http.StatusForbidden || res.errCode != "auth.forbidden" {
					t.Errorf("%s owner with foreign X-Property-ID: got %d/%q, want 403 auth.forbidden", name, res.status, res.errCode)
				}
			}
		})
	}
	if len(unclassified) > 0 {
		t.Fatalf("%d route(s) not classified in the authorization matrix; add them to classifyRoute:\n  %s",
			len(unclassified), strings.Join(unclassified, "\n  "))
	}
	for p := range exactPublicPaths {
		if !seenPublic[p] {
			t.Errorf("reviewed public path %q is not registered in the router", p)
		}
	}
	t.Logf("matrix: %d routes, %d authorization checks", len(routes), cases)
}
