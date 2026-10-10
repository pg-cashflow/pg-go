package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	hazards     []domain.HazardReport
	activePoll  *domain.MenuPoll
	balance     int
	streak      *domain.TenantStreak
	redemptions []domain.Redemption
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
	if m.activePoll != nil && m.activePoll.PropertyID == propertyID {
		return m.activePoll, nil
	}
	return nil, nil
}

func (m *mockGamificationStore) GetHazardByID(ctx context.Context, id uuid.UUID) (*domain.HazardReport, error) {
	for _, h := range m.hazards {
		if h.ID == id {
			return &h, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (m *mockGamificationStore) GetRewardByID(ctx context.Context, id uuid.UUID) (*domain.RewardsCatalogItem, error) {
	for _, r := range m.catalog {
		if r.ID == id {
			return &r, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (m *mockGamificationStore) ListFloors(ctx context.Context, propertyID uuid.UUID) ([]domain.Floor, error) {
	var out []domain.Floor
	for _, f := range m.floors {
		if f.PropertyID == propertyID {
			out = append(out, f)
		}
	}
	return out, nil
}

func (m *mockGamificationStore) ListRooms(ctx context.Context, propertyID uuid.UUID) ([]domain.Room, error) {
	var out []domain.Room
	for _, r := range m.rooms {
		if r.PropertyID == propertyID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *mockGamificationStore) ListReferralsByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Referral, error) {
	return nil, nil
}

func (m *mockGamificationStore) ListPointRules(ctx context.Context, propertyID uuid.UUID) ([]domain.PointRule, error) {
	return []domain.PointRule{
		{Code: "RENT_ON_TIME", Points: 50, Active: true},
	}, nil
}

func (m *mockGamificationStore) ListRedemptionsByProperty(ctx context.Context, propertyID uuid.UUID, status string) ([]domain.Redemption, error) {
	var out []domain.Redemption
	for _, r := range m.redemptions {
		if r.PropertyID == propertyID && (status == "" || r.Status == status) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (m *mockGamificationStore) GetRedemptionByID(ctx context.Context, propertyID uuid.UUID, id uuid.UUID) (*domain.Redemption, error) {
	for _, r := range m.redemptions {
		if r.PropertyID == propertyID && r.ID == id {
			return &r, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (m *mockGamificationStore) FulfilRedemption(ctx context.Context, propertyID uuid.UUID, id uuid.UUID) error {
	for i, r := range m.redemptions {
		if r.PropertyID == propertyID && r.ID == id {
			if r.Status != "pending" {
				return errors.New("cannot fulfil non-pending redemption")
			}
			m.redemptions[i].Status = "fulfilled"
			return nil
		}
	}
	return domain.ErrNotFound
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
	req := httptest.NewRequest(http.MethodGet, "/api/manager/hazards?property_id="+propA.String(), nil)
	req.Header.Set("Authorization", "Bearer "+tenantToken)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for tenant accessing manager endpoint, got %d", w.Code)
	}

	// 2. Manager trying to call owner-only endpoint -> 403
	req2 := httptest.NewRequest(http.MethodGet, "/api/owner/gamification/settings?property_id="+propA.String(), nil)
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
	req3 := httptest.NewRequest(http.MethodPost, "/api/manager/inspections", bytes.NewReader(b))
	req3.Header.Set("Authorization", "Bearer "+managerAToken)
	req3.Header.Set("Content-Type", "application/json")
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, req3)
	if w3.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for manager cross-property inspection, got %d", w3.Code)
	}

	// 4. Owner can access owner endpoint -> 200
	req4 := httptest.NewRequest(http.MethodGet, "/api/owner/gamification/settings?property_id="+propA.String(), nil)
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

func TestGamificationCrossPropertyIDORGuards(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test-secret-key-with-sufficient-length-32"

	propA := uuid.New()
	propB := uuid.New()

	tenantA := &domain.Tenant{ID: uuid.New(), PropertyID: propA, Status: domain.TenantStatusActive}
	tenantB := &domain.Tenant{ID: uuid.New(), PropertyID: propB, Status: domain.TenantStatusActive}

	ownerTokenA, _ := auth.IssueToken(jwtSecret, &domain.User{ID: uuid.New(), Role: domain.RoleOwner, PropertyID: &propA})
	managerTokenA, _ := auth.IssueToken(jwtSecret, &domain.User{ID: uuid.New(), Role: domain.RoleManager, PropertyID: &propA})
	tenantTokenA, _ := auth.IssueToken(jwtSecret, &domain.User{ID: uuid.New(), Role: domain.RoleTenant, TenantID: &tenantA.ID, PropertyID: &propA})

	rewardB := domain.RewardsCatalogItem{
		ID:         uuid.New(),
		PropertyID: propB,
		Title:      "Reward B",
		PointsCost: 50,
		IsActive:   true,
	}

	pollB := &domain.MenuPoll{
		ID:         uuid.New(),
		PropertyID: propB,
		MonthYear:  time.Now().UTC().Format("2006-01"),
		Title:      "Poll B",
	}

	hazardB := domain.HazardReport{
		ID:         uuid.New(),
		PropertyID: propB,
		Status:     "reported",
	}

	floorB := domain.Floor{
		ID:         uuid.New(),
		PropertyID: propB,
		Name:       "Floor 1",
	}

	store := &mockGamificationStore{
		catalog:    []domain.RewardsCatalogItem{rewardB},
		activePoll: pollB,
		hazards:    []domain.HazardReport{hazardB},
		floors:     []domain.Floor{floorB},
	}

	deps := Deps{
		JWTSecret:         jwtSecret,
		GamificationStore: store,
		TenantStore:       &multiTenantStoreStub{tenants: map[uuid.UUID]*domain.Tenant{tenantA.ID: tenantA, tenantB.ID: tenantB}},
		AuthTenantRepo:    &multiTenantRepoStub{tenants: map[uuid.UUID]*domain.Tenant{tenantA.ID: tenantA, tenantB.ID: tenantB}},
	}
	router := NewRouter(deps)

	// 1. Tenant A tries to redeem Property B reward -> 404
	req := httptest.NewRequest(http.MethodPost, "/api/tenant/rewards/"+rewardB.ID.String()+"/redeem", nil)
	req.Header.Set("Authorization", "Bearer "+tenantTokenA)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for tenant redeeming cross-property reward, got %d", w.Code)
	}

	// 2. Tenant A tries to vote on Property B poll -> 404
	pollVoteBody, _ := json.Marshal(map[string]any{"poll_id": pollB.ID.String(), "option_id": "opt1"})
	req2 := httptest.NewRequest(http.MethodPost, "/api/tenant/menu-poll/vote", bytes.NewReader(pollVoteBody))
	req2.Header.Set("Authorization", "Bearer "+tenantTokenA)
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	router.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for tenant voting on cross-property poll, got %d", w2.Code)
	}

	// 3. Manager A tries to query inspections of Property B -> 403
	req3 := httptest.NewRequest(http.MethodGet, "/api/manager/inspections?property_id="+propB.String(), nil)
	req3.Header.Set("Authorization", "Bearer "+managerTokenA)
	w3 := httptest.NewRecorder()
	router.ServeHTTP(w3, req3)
	if w3.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for manager listing cross-property inspections, got %d", w3.Code)
	}

	// 4. Manager A tries to log violation against Tenant B -> 404
	violBody, _ := json.Marshal(map[string]any{
		"tenant_id":   tenantB.ID.String(),
		"rule_code":   "NOISE",
		"severity":    "lifestyle",
		"description": "late night noise",
	})
	req4 := httptest.NewRequest(http.MethodPost, "/api/manager/violations", bytes.NewReader(violBody))
	req4.Header.Set("Authorization", "Bearer "+managerTokenA)
	req4.Header.Set("Content-Type", "application/json")
	w4 := httptest.NewRecorder()
	router.ServeHTTP(w4, req4)
	if w4.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for manager logging violation on cross-property tenant, got %d", w4.Code)
	}

	// 5. Manager A tries to record meter reading for Property B -> 403
	meterBody, _ := json.Marshal(map[string]any{
		"property_id":   propB.String(),
		"kind":          "electricity",
		"reading_value": 123.4,
	})
	req5 := httptest.NewRequest(http.MethodPost, "/api/manager/meter-readings", bytes.NewReader(meterBody))
	req5.Header.Set("Authorization", "Bearer "+managerTokenA)
	req5.Header.Set("Content-Type", "application/json")
	w5 := httptest.NewRecorder()
	router.ServeHTTP(w5, req5)
	if w5.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for manager recording meter reading on cross-property, got %d", w5.Code)
	}

	// 6. Manager A tries to view kitchen headcount for Property B -> 403
	req6 := httptest.NewRequest(http.MethodGet, "/api/manager/kitchen/headcount?property_id="+propB.String(), nil)
	req6.Header.Set("Authorization", "Bearer "+managerTokenA)
	w6 := httptest.NewRecorder()
	router.ServeHTTP(w6, req6)
	if w6.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for manager viewing cross-property headcount, got %d", w6.Code)
	}

	// 7. Manager A tries to list hazards for Property B -> 403
	req7 := httptest.NewRequest(http.MethodGet, "/api/manager/hazards?property_id="+propB.String(), nil)
	req7.Header.Set("Authorization", "Bearer "+managerTokenA)
	w7 := httptest.NewRecorder()
	router.ServeHTTP(w7, req7)
	if w7.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for manager listing cross-property hazards, got %d", w7.Code)
	}

	// 8. Manager A tries to resolve Hazard B -> 404
	req8 := httptest.NewRequest(http.MethodPost, "/api/manager/hazards/"+hazardB.ID.String()+"/resolve", bytes.NewBufferString("{}"))
	req8.Header.Set("Authorization", "Bearer "+managerTokenA)
	req8.Header.Set("Content-Type", "application/json")
	w8 := httptest.NewRecorder()
	router.ServeHTTP(w8, req8)
	if w8.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for manager resolving cross-property hazard, got %d", w8.Code)
	}

	// 9. Manager A tries to submit vendor inspection for Property B -> 403
	vendorBody, _ := json.Marshal(map[string]any{
		"property_id":   propB.String(),
		"vendor_name":   "Vendor 1",
		"score_percent": 80,
	})
	req9 := httptest.NewRequest(http.MethodPost, "/api/manager/vendor-inspections", bytes.NewReader(vendorBody))
	req9.Header.Set("Authorization", "Bearer "+managerTokenA)
	req9.Header.Set("Content-Type", "application/json")
	w9 := httptest.NewRecorder()
	router.ServeHTTP(w9, req9)
	if w9.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for manager submitting vendor inspection on cross-property, got %d", w9.Code)
	}

	// 10. Owner A tries to get gamification settings for Property B -> 403
	req10 := httptest.NewRequest(http.MethodGet, "/api/owner/gamification/settings?property_id="+propB.String(), nil)
	req10.Header.Set("Authorization", "Bearer "+ownerTokenA)
	w10 := httptest.NewRecorder()
	router.ServeHTTP(w10, req10)
	if w10.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for owner accessing cross-property gamification settings, got %d", w10.Code)
	}

	// 11. Owner A tries to list floors for Property B -> 403
	req11 := httptest.NewRequest(http.MethodGet, "/api/owner/floors?property_id="+propB.String(), nil)
	req11.Header.Set("Authorization", "Bearer "+ownerTokenA)
	w11 := httptest.NewRecorder()
	router.ServeHTTP(w11, req11)
	if w11.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for owner listing cross-property floors, got %d", w11.Code)
	}

	// 12. Owner A tries to list rooms for Property B -> 403
	req12 := httptest.NewRequest(http.MethodGet, "/api/owner/rooms?property_id="+propB.String(), nil)
	req12.Header.Set("Authorization", "Bearer "+ownerTokenA)
	w12 := httptest.NewRecorder()
	router.ServeHTTP(w12, req12)
	if w12.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for owner listing cross-property rooms, got %d", w12.Code)
	}

	// 13. Owner A tries to create a room referencing Floor B (belongs to Property B) -> 400
	roomBody, _ := json.Marshal(map[string]any{
		"floor_id":    floorB.ID.String(),
		"room_number": "101",
		"capacity":    2,
	})
	req13 := httptest.NewRequest(http.MethodPost, "/api/owner/rooms", bytes.NewReader(roomBody))
	req13.Header.Set("Authorization", "Bearer "+ownerTokenA)
	req13.Header.Set("Content-Type", "application/json")
	w13 := httptest.NewRecorder()
	router.ServeHTTP(w13, req13)
	if w13.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for owner creating room with foreign floor, got %d", w13.Code)
	}

	// 14. Owner A tries to create manager assigned to Property B -> 403
	mgrBody, _ := json.Marshal(map[string]any{
		"phone":       "+919876543210",
		"property_id": propB.String(),
	})
	req14 := httptest.NewRequest(http.MethodPost, "/api/owner/managers", bytes.NewReader(mgrBody))
	req14.Header.Set("Authorization", "Bearer "+ownerTokenA)
	req14.Header.Set("Content-Type", "application/json")
	w14 := httptest.NewRecorder()
	router.ServeHTTP(w14, req14)
	if w14.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for owner creating manager for foreign property, got %d", w14.Code)
	}
}

type multiTenantRepoStub struct {
	tenants map[uuid.UUID]*domain.Tenant
}

func (s *multiTenantRepoStub) GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if t, ok := s.tenants[id]; ok {
		return t, nil
	}
	return nil, domain.ErrNotFound
}

func (s *multiTenantRepoStub) GetByPhone(ctx context.Context, phone string) (*domain.Tenant, error) {
	return nil, domain.ErrNotFound
}

func (s *multiTenantRepoStub) Update(ctx context.Context, t *domain.Tenant) error {
	s.tenants[t.ID] = t
	return nil
}

type multiTenantStoreStub struct {
	tenants map[uuid.UUID]*domain.Tenant
}

func (s *multiTenantStoreStub) GetByID(ctx context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if t, ok := s.tenants[id]; ok {
		return t, nil
	}
	return nil, domain.ErrNotFound
}

func (s *multiTenantStoreStub) GetByPhone(ctx context.Context, phone string) (*domain.Tenant, error) {
	return nil, domain.ErrNotFound
}

func (s *multiTenantStoreStub) Create(ctx context.Context, t *domain.Tenant) error { return nil }
func (s *multiTenantStoreStub) Update(ctx context.Context, t *domain.Tenant) error { return nil }
func (s *multiTenantStoreStub) ListByProperty(ctx context.Context, propertyID uuid.UUID) ([]domain.Tenant, error) {
	return nil, nil
}
func (s *multiTenantStoreStub) SetNotice(ctx context.Context, tenantID uuid.UUID, noticeDate time.Time) error {
	return nil
}
func (s *multiTenantStoreStub) SetVacated(ctx context.Context, tenantID uuid.UUID) error { return nil }
func (s *multiTenantStoreStub) AttachPhone(ctx context.Context, tenantID uuid.UUID, phone string) error {
	return nil
}
func (s *multiTenantStoreStub) SaveIDPhoto(ctx context.Context, tenantID uuid.UUID, photo []byte) error {
	return nil
}
func (s *multiTenantStoreStub) GetIDPhoto(ctx context.Context, tenantID uuid.UUID) ([]byte, error) {
	return nil, nil
}
func (s *multiTenantStoreStub) CountActiveByRoom(ctx context.Context, roomNumber string) (int, error) {
	return 0, nil
}

func TestOwnerRedemptions_ListAndFulfil(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test-secret-key-with-sufficient-length-32"

	propA := uuid.New()
	propB := uuid.New()
	tenantA := &domain.Tenant{ID: uuid.New(), PropertyID: propA, Status: domain.TenantStatusActive}

	ownerTokenA, _ := auth.IssueToken(jwtSecret, &domain.User{ID: uuid.New(), Role: domain.RoleOwner, PropertyID: &propA})
	ownerTokenB, _ := auth.IssueToken(jwtSecret, &domain.User{ID: uuid.New(), Role: domain.RoleOwner, PropertyID: &propB})

	couponCode := "FOOD-TEST1234"
	redPending := domain.Redemption{
		ID:          uuid.New(),
		TenantID:    tenantA.ID,
		PropertyID:  propA,
		RewardID:    uuid.New(),
		PointsSpent: 100,
		Status:      "pending",
		CouponCode:  &couponCode,
		CreatedAt:   time.Now().UTC(),
	}

	store := &mockGamificationStore{
		redemptions: []domain.Redemption{redPending},
	}
	gamSvc := gamification.NewService(store, &multiTenantRepoStub{tenants: map[uuid.UUID]*domain.Tenant{tenantA.ID: tenantA}}, nil, nil, nil)

	deps := Deps{
		JWTSecret:         jwtSecret,
		Gamification:      gamSvc,
		GamificationStore: store,
		TenantStore:       &stubTenantStore{t: tenantA},
		AuthTenantRepo:    &stubTenantRepo{t: tenantA},
	}
	router := NewRouter(deps)

	// 1. Owner A lists redemptions -> 200 with 1 pending item
	req := httptest.NewRequest(http.MethodGet, "/api/owner/gamification/redemptions", nil)
	req.Header.Set("Authorization", "Bearer "+ownerTokenA)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", w.Code, w.Body.String())
	}

	var listResp struct {
		Redemptions []domain.Redemption `json:"redemptions"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(listResp.Redemptions) != 1 || listResp.Redemptions[0].ID != redPending.ID {
		t.Fatalf("expected 1 redemption, got %d", len(listResp.Redemptions))
	}

	// 2. Owner B tries to fulfil Owner A's redemption -> 400 (not found for propB)
	reqB := httptest.NewRequest(http.MethodPost, "/api/owner/gamification/redemptions/"+redPending.ID.String()+"/fulfil", nil)
	reqB.Header.Set("Authorization", "Bearer "+ownerTokenB)
	wB := httptest.NewRecorder()
	router.ServeHTTP(wB, reqB)
	if wB.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for cross-property fulfilment, got %d", wB.Code)
	}

	// 3. Owner A fulfils pending redemption -> 200 with fulfilled status
	reqA := httptest.NewRequest(http.MethodPost, "/api/owner/gamification/redemptions/"+redPending.ID.String()+"/fulfil", nil)
	reqA.Header.Set("Authorization", "Bearer "+ownerTokenA)
	wA := httptest.NewRecorder()
	router.ServeHTTP(wA, reqA)
	if wA.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body: %s)", wA.Code, wA.Body.String())
	}

	var fulfilResp struct {
		Redemption domain.Redemption `json:"redemption"`
	}
	if err := json.Unmarshal(wA.Body.Bytes(), &fulfilResp); err != nil {
		t.Fatalf("failed to decode fulfil response: %v", err)
	}
	if fulfilResp.Redemption.Status != "fulfilled" {
		t.Fatalf("expected status 'fulfilled', got '%s'", fulfilResp.Redemption.Status)
	}
}
