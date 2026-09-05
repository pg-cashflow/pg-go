package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/gamification"
)

type mockGamificationStore struct {
	gamification.Store
	floors      []domain.Floor
	rooms       []domain.Room
	inspections []domain.Inspection
	violations  []domain.Violation
	catalog     []domain.RewardsCatalogItem
	balance     int
	streak      *domain.TenantStreak
}

func (m *mockGamificationStore) GetSettings(ctx context.Context, propertyID uuid.UUID) (*domain.PropertyGamificationSettings, error) {
	return &domain.PropertyGamificationSettings{
		PropertyID:          propertyID,
		PointValuePaise:     100,
		EarnCapPerTenant:    200,
		RSVPSubCap:          60,
		FloorBonusThreshold: 85,
	}, nil
}

func (m *mockGamificationStore) GetActiveBalance(ctx context.Context, tenantID uuid.UUID) (int, error) {
	return m.balance, nil
}

func (m *mockGamificationStore) GetExpiringSoon(ctx context.Context, tenantID uuid.UUID, withinDays int) (int, time.Time, error) {
	return 0, time.Time{}, nil
}

func (m *mockGamificationStore) GetStreak(ctx context.Context, tenantID uuid.UUID) (*domain.TenantStreak, error) {
	if m.streak == nil {
		return &domain.TenantStreak{TenantID: tenantID, FreezesAvailable: 1}, nil
	}
	return m.streak, nil
}

func (m *mockGamificationStore) ListLedgerByTenant(ctx context.Context, tenantID uuid.UUID, limit int) ([]domain.PointsLedgerEntry, error) {
	return nil, nil
}

func (m *mockGamificationStore) ListRewardsCatalog(ctx context.Context, propertyID uuid.UUID) ([]domain.RewardsCatalogItem, error) {
	return m.catalog, nil
}

func (m *mockGamificationStore) CountStep3ViolationsInQuarter(ctx context.Context, tenantID uuid.UUID) (int, error) {
	return 0, nil
}

func (m *mockGamificationStore) ListViolationsByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Violation, error) {
	var out []domain.Violation
	for _, v := range m.violations {
		if v.TenantID == tenantID {
			out = append(out, v)
		}
	}
	return out, nil
}

func (m *mockGamificationStore) ListInspections(ctx context.Context, propertyID uuid.UUID, roomID *uuid.UUID, floorID *uuid.UUID, limit int) ([]domain.Inspection, error) {
	return m.inspections, nil
}

func (m *mockGamificationStore) GetTopStreaks(ctx context.Context, propertyID uuid.UUID, limit int) ([]domain.TenantStreak, error) {
	return nil, nil
}

func (m *mockGamificationStore) GetFloorCleanScores(ctx context.Context, propertyID uuid.UUID, monthYear string) (map[uuid.UUID]float64, error) {
	return nil, nil
}

func (m *mockGamificationStore) GetTenantMealRSVP(ctx context.Context, tenantID uuid.UUID, date time.Time, slot string) (*domain.MealRSVP, error) {
	return nil, nil
}

func (m *mockGamificationStore) GetActiveMenuPoll(ctx context.Context, propertyID uuid.UUID, monthYear string) (*domain.MenuPoll, error) {
	return nil, nil
}

func (m *mockGamificationStore) ListReferralsByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Referral, error) {
	return nil, nil
}

func (m *mockGamificationStore) ListPointRules(ctx context.Context, propertyID uuid.UUID) ([]domain.PointRule, error) {
	return []domain.PointRule{
		{Code: "RENT_ON_TIME", Points: 50, Active: true},
	}, nil
}

// TestAuthorizationMatrix verifies role boundaries:
// 1. Tenant cannot access manager inspection routes.
// 2. Manager cannot access owner-only routes.
// 3. Manager cannot inspect another property.
func TestAuthorizationMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test-secret-key-with-sufficient-length-32"

	propA := uuid.New()
	propB := uuid.New()
	tenantA := &domain.Tenant{ID: uuid.New(), PropertyID: propA, Status: domain.TenantStatusActive}

	ownerToken, _ := auth.IssueToken(jwtSecret, &domain.User{ID: uuid.New(), Role: domain.RoleOwner, PropertyID: &propA})
	managerAToken, _ := auth.IssueToken(jwtSecret, &domain.User{ID: uuid.New(), Role: domain.RoleManager, PropertyID: &propA})
	tenantToken, _ := auth.IssueToken(jwtSecret, &domain.User{ID: uuid.New(), Role: domain.RoleTenant, TenantID: &tenantA.ID, PropertyID: &propA})

	store := &mockGamificationStore{}
	deps := Deps{
		JWTSecret:         jwtSecret,
		GamificationStore: store,
		TenantStore:       &stubTenantStore{t: tenantA},
		AuthTenantRepo:    &stubTenantRepo{t: tenantA},
	}
	router := NewRouter(deps)

	// 1. Tenant trying to call manager endpoint -> 403
	req := httptest.NewRequest(http.MethodGet, "/manager/hazards?property_id="+propA.String(), nil)
	req.Header.Set("Authorization", "Bearer "+tenantToken)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for tenant accessing manager endpoint, got %d", w.Code)
	}

	// 2. Manager trying to call owner-only endpoint -> 403
	req2 := httptest.NewRequest(http.MethodGet, "/owner/gamification/settings?property_id="+propA.String(), nil)
	req2.Header.Set("Authorization", "Bearer "+managerAToken)
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for manager accessing owner endpoint, got %d", w2.Code)
	}

	// 3. Manager A trying to inspect Property B -> 403
	inspBody := map[string]any{
		"property_id":     propB.String(),
		"inspection_type": "floor",
		"items":           []map[string]any{},
	}
	b, _ := json.Marshal(inspBody)
	req3 := httptest.NewRequest(http.MethodPost, "/manager/inspections", bytes.NewReader(b))
	req3.Header.Set("Authorization", "Bearer "+managerAToken)
	req3.Header.Set("Content-Type", "application/json")
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, req3)
	if w3.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for manager cross-property inspection, got %d", w3.Code)
	}

	// 4. Owner can access owner endpoint -> 200
	req4 := httptest.NewRequest(http.MethodGet, "/owner/gamification/settings?property_id="+propA.String(), nil)
	req4.Header.Set("Authorization", "Bearer "+ownerToken)
	w4 := httptest.NewRecorder()
	router.ServeHTTP(w4, req4)
	if w4.Code != http.StatusOK {
		t.Fatalf("expected 200 for owner accessing owner endpoint, got %d", w4.Code)
	}
}

type stubTenantRepo struct {
	t *domain.Tenant
}

func (s *stubTenantRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return s.t, nil
}

func (s *stubTenantRepo) GetByPhone(ctx context.Context, phone string) (*domain.Tenant, error) {
	return s.t, nil
}
