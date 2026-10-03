package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/search"
)

func TestSearchRBAC_tenantScopedRepo(t *testing.T) {
	jwtSecret := "test-secret-key-with-sufficient-length-32"
	propID := uuid.New()
	tenantID := uuid.New()
	otherTenant := uuid.New()

	tenantUser := &domain.User{ID: uuid.New(), Role: domain.RoleTenant, PropertyID: &propID, TenantID: &tenantID, TokenVersion: 1}
	store := &stubSessionStore{user: tenantUser}
	tenantToken, err := auth.IssueToken(jwtSecret, tenantUser)
	if err != nil {
		t.Fatal(err)
	}

	searchSvc := &search.Service{Repo: &idORRepo{otherTenantID: otherTenant}}
	r := NewRouter(Deps{
		JWTSecret:    jwtSecret,
		AuthUserRepo: store,
		SearchSvc:    searchSvc,
	})

	req := httptest.NewRequest(http.MethodGet, "/api/search?q=ab", nil)
	req.Header.Set("Authorization", "Bearer "+tenantToken)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("tenant status=%d body=%s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	results, _ := body["results"].([]any)
	if len(results) != 0 {
		t.Fatalf("expected no cross-tenant leak, got %d results", len(results))
	}

	// 101-character query returns 400 Bad Request (not 500)
	longQ := strings.Repeat("a", 101)
	reqLong := httptest.NewRequest(http.MethodGet, "/api/search?q="+longQ, nil)
	reqLong.Header.Set("Authorization", "Bearer "+tenantToken)
	wLong := httptest.NewRecorder()
	r.ServeHTTP(wLong, reqLong)
	if wLong.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for 101-character query, got %d body=%s", wLong.Code, wLong.Body.String())
	}

	// 1-character query returns 400 Bad Request
	reqShort := httptest.NewRequest(http.MethodGet, "/api/search?q=a", nil)
	reqShort.Header.Set("Authorization", "Bearer "+tenantToken)
	wShort := httptest.NewRecorder()
	r.ServeHTTP(wShort, reqShort)
	if wShort.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for 1-character query, got %d body=%s", wShort.Code, wShort.Body.String())
	}
}

// idORRepo returns a hit only when tenant scope matches otherTenantID (simulated leak if mis-scoped).
type idORRepo struct {
	otherTenantID uuid.UUID
}

func (r *idORRepo) SearchLexical(_ context.Context, p search.Params, types []search.EntityType, _ int) ([]search.Result, error) {
	if p.TenantID != nil && *p.TenantID == r.otherTenantID {
		return []search.Result{{Type: search.TypeDue, ID: "leak", Title: "secret"}}, nil
	}
	return nil, nil
}

func (r *idORRepo) SearchVector(context.Context, search.Params, int, []float32) ([]search.Result, error) {
	return nil, nil
}
