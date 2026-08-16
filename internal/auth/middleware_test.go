package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
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
	r.GET("/me", RequireTenant(secret, repo), func(c *gin.Context) {
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
	r.GET("/me", RequireTenant(secret, repo), func(c *gin.Context) {
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
