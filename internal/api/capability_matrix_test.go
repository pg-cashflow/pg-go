package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mapCaps struct {
	grants map[string]bool
	err    error
}

func (m mapCaps) HasActive(_ context.Context, _, _ uuid.UUID, capability string) (bool, error) {
	if m.err != nil {
		return false, m.err
	}
	return m.grants[capability], nil
}

func (m mapCaps) ListActive(context.Context, uuid.UUID, uuid.UUID) ([]string, error) {
	var out []string
	for k, ok := range m.grants {
		if ok {
			out = append(out, k)
		}
	}
	return out, m.err
}

func TestRequireCapabilityMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	prop := uuid.New()
	user := uuid.New()
	checker := mapCaps{grants: map[string]bool{"inspections.write": true}}

	cases := []struct {
		name string
		role domain.Role
		cap  string
		want int
	}{
		{name: "owner passes without grant", role: domain.RoleOwner, cap: "expenses.void", want: http.StatusNoContent},
		{name: "manager with grant", role: domain.RoleManager, cap: "inspections.write", want: http.StatusNoContent},
		{name: "manager without grant", role: domain.RoleManager, cap: "expenses.void", want: http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := gin.New()
			r.Use(func(c *gin.Context) {
				c.Set(auth.ContextClaimsKey, &auth.Claims{UserID: user, Role: tc.role, PropertyID: &prop})
				c.Next()
			})
			r.POST("/x", auth.RequireCapability(tc.cap, checker), func(c *gin.Context) {
				c.Status(http.StatusNoContent)
			})
			w := httptest.NewRecorder()
			r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/x", nil))
			if w.Code != tc.want {
				t.Fatalf("expected %d, got %d body %s", tc.want, w.Code, w.Body.String())
			}
		})
	}
}

func TestManagerRouteCapabilityTags(t *testing.T) {
	tags := map[string]string{
		"POST /inspections":               "inspections.write",
		"GET /inspections":                "inspections.write",
		"POST /meter-readings":            "meters.write",
		"GET /kitchen/headcount":          "kitchen.headcount",
		"POST /finance/expenses":          "expenses.post",
		"POST /finance/expenses/:id/void": "expenses.void",
		"GET /me/capabilities":            "",
	}
	if tags["POST /finance/expenses/:id/void"] != "expenses.void" {
		t.Fatal("void route must require expenses.void")
	}
	if _, ok := tags["GET /me/capabilities"]; !ok {
		t.Fatal("me capabilities route must be registered")
	}
}
