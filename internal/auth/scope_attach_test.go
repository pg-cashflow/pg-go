package auth

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

// attachScopeFor runs AttachRequestPropertyScope against a request carrying the
// given header and query, and returns the property ID the request scope ends up with.
func attachScopeFor(t *testing.T, claims *Claims, header, query string) (uuid.UUID, bool) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	url := "/x"
	if query != "" {
		url += "?property_id=" + query
	}
	req := httptest.NewRequest("GET", url, nil)
	if header != "" {
		req.Header.Set("X-Property-ID", header)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	AttachRequestPropertyScope(c, claims)
	return requestscope.PropertyIDFromContext(c.Request.Context())
}

// An owner whose token has no property_id must not gain a scope from a header or
// query parameter. Scope is only ever established by verified resolution.
func TestAttachRequestPropertyScope_OwnerWithoutClaimIgnoresHeaderAndQuery(t *testing.T) {
	victim := uuid.New()
	claims := &Claims{UserID: uuid.New(), Role: domain.RoleOwner}

	if got, ok := attachScopeFor(t, claims, victim.String(), ""); ok {
		t.Fatalf("header set scope to %s for an owner with no property claim", got)
	}
	if got, ok := attachScopeFor(t, claims, "", victim.String()); ok {
		t.Fatalf("query set scope to %s for an owner with no property claim", got)
	}
}

// The token's own property claim is the only source of scope, for every role.
func TestAttachRequestPropertyScope_UsesClaimAndIgnoresHeaderForAllRoles(t *testing.T) {
	own := uuid.New()
	other := uuid.New()
	for _, role := range []domain.Role{domain.RoleOwner, domain.RoleManager, domain.RoleTenant} {
		claims := &Claims{UserID: uuid.New(), Role: role, PropertyID: &own}
		got, ok := attachScopeFor(t, claims, other.String(), other.String())
		if !ok || got != own {
			t.Fatalf("role %s: scope = %v (ok=%v), want claim property %s", role, got, ok, own)
		}
	}
}

// A nil claims value or a nil property must leave the context unscoped.
func TestAttachRequestPropertyScope_NoClaimLeavesContextUnscoped(t *testing.T) {
	if _, ok := attachScopeFor(t, nil, uuid.New().String(), ""); ok {
		t.Fatal("nil claims produced a scope")
	}
	nilProp := uuid.Nil
	claims := &Claims{UserID: uuid.New(), Role: domain.RoleManager, PropertyID: &nilProp}
	if _, ok := attachScopeFor(t, claims, "", ""); ok {
		t.Fatal("nil-UUID property claim produced a scope")
	}
}
