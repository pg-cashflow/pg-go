package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/gamification"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

// In-memory test stubs for dashboard testing
type dashboardDueStore struct {
	dues map[uuid.UUID]*domain.Due
}

func (s *dashboardDueStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Due, error) {
	d := s.dues[id]
	if d == nil {
		return nil, fmt.Errorf("due not found")
	}
	return d, nil
}

func (s *dashboardDueStore) ListByProperty(_ context.Context, propID uuid.UUID) ([]domain.Due, error) {
	var out []domain.Due
	for _, d := range s.dues {
		if d.PropertyID == propID {
			out = append(out, *d)
		}
	}
	return out, nil
}

func (s *dashboardDueStore) List(_ context.Context, f postgres.DueListFilter) ([]domain.Due, error) {
	var out []domain.Due
	for _, d := range s.dues {
		if d.PropertyID == f.PropertyID {
			out = append(out, *d)
		}
	}
	return out, nil
}

func (s *dashboardDueStore) ListByTenant(context.Context, uuid.UUID) ([]domain.Due, error) {
	return nil, nil
}

type dashboardTenantStore struct {
	tenants map[uuid.UUID]*domain.Tenant
}

func (s *dashboardTenantStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
	t := s.tenants[id]
	if t == nil {
		return nil, fmt.Errorf("tenant not found")
	}
	return t, nil
}

func (s *dashboardTenantStore) ListByProperty(_ context.Context, propID uuid.UUID) ([]domain.Tenant, error) {
	var out []domain.Tenant
	for _, t := range s.tenants {
		if t.PropertyID == propID {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (s *dashboardTenantStore) Update(context.Context, *domain.Tenant) error {
	return nil
}

func (s *dashboardTenantStore) GetIDPhoto(context.Context, uuid.UUID) ([]byte, error) {
	return nil, nil
}

type dashboardPropStore struct {
	props    map[uuid.UUID]*domain.Property
	settings map[uuid.UUID]*domain.PropertySettings
}

func (s *dashboardPropStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Property, error) {
	p := s.props[id]
	if p == nil {
		return nil, fmt.Errorf("property not found")
	}
	return p, nil
}

func (s *dashboardPropStore) List(context.Context) ([]domain.Property, error) {
	return nil, nil
}

func (s *dashboardPropStore) GetByOwnerPhone(context.Context, string) (*domain.Property, error) {
	return nil, nil
}

func (s *dashboardPropStore) GetByInviteCode(context.Context, string) (*domain.Property, error) {
	return nil, nil
}

func (s *dashboardPropStore) Create(_ context.Context, p *domain.Property) error {
	if s.props == nil {
		s.props = make(map[uuid.UUID]*domain.Property)
	}
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	s.props[p.ID] = p
	return nil
}

func (s *dashboardPropStore) GetSettings(_ context.Context, pid uuid.UUID) (*domain.PropertySettings, error) {
	if s.settings != nil && s.settings[pid] != nil {
		return s.settings[pid], nil
	}
	st := domain.DefaultPropertySettings(pid)
	return &st, nil
}

func (s *dashboardPropStore) UpsertSettings(_ context.Context, st *domain.PropertySettings) error {
	if s.settings == nil {
		s.settings = make(map[uuid.UUID]*domain.PropertySettings)
	}
	s.settings[st.PropertyID] = st
	return nil
}

type dashboardGamificationStore struct {
	gamification.Store
	floors []domain.Floor
	rooms  []domain.Room
}

func (s *dashboardGamificationStore) ListFloors(_ context.Context, _ uuid.UUID) ([]domain.Floor, error) {
	return s.floors, nil
}

func (s *dashboardGamificationStore) ListRooms(_ context.Context, _ uuid.UUID) ([]domain.Room, error) {
	return s.rooms, nil
}

func TestOwnerOccupancy_ComputesCorrectMetrics(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test_secret_for_occupancy"
	propID := uuid.New()
	userID := uuid.New()

	ownerUser := &domain.User{
		ID:         userID,
		PropertyID: &propID,
		Role:       domain.RoleOwner,
	}
	token, err := auth.IssueToken(jwtSecret, ownerUser)
	if err != nil {
		t.Fatalf("IssueToken failed: %v", err)
	}

	tenants := map[uuid.UUID]*domain.Tenant{
		uuid.New(): {ID: uuid.New(), PropertyID: propID, Status: domain.TenantStatusActive},
		uuid.New(): {ID: uuid.New(), PropertyID: propID, Status: domain.TenantStatusActive},
		uuid.New(): {ID: uuid.New(), PropertyID: propID, Status: domain.TenantStatusVacated}, // Ignored
	}

	h := &Handlers{
		Deps: Deps{
			JWTSecret:   jwtSecret,
			TenantStore: &dashboardTenantStore{tenants: tenants},
		},
	}

	r := gin.New()
	ownerGroup := r.Group("/owner", auth.RequireOwner(jwtSecret, nil))
	ownerGroup.GET("/occupancy", h.OwnerOccupancy)

	req := httptest.NewRequest(http.MethodGet, "/owner/occupancy", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var res struct {
		OccupiedBeds int     `json:"occupied_beds"`
		VacantBeds   int     `json:"vacant_beds"`
		RatePct      float64 `json:"occupancy_rate_pct"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	if res.OccupiedBeds != 2 {
		t.Errorf("expected 2 active occupied beds, got %d", res.OccupiedBeds)
	}
}

func TestOwnerOccupancy_FloorWiseAndVacantRoomsTieOut(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test_secret_for_floor_occupancy"
	propID := uuid.New()
	userID := uuid.New()

	ownerUser := &domain.User{
		ID:         userID,
		PropertyID: &propID,
		Role:       domain.RoleOwner,
	}
	token, err := auth.IssueToken(jwtSecret, ownerUser)
	if err != nil {
		t.Fatalf("IssueToken failed: %v", err)
	}

	floor1ID := uuid.New()
	floor2ID := uuid.New()

	floors := []domain.Floor{
		{ID: floor1ID, PropertyID: propID, FloorNumber: 1, Name: "Ground Floor"},
		{ID: floor2ID, PropertyID: propID, FloorNumber: 2, Name: "First Floor"},
	}

	r101ID := uuid.New()
	r102ID := uuid.New()
	r103ID := uuid.New()
	r201ID := uuid.New()
	r202ID := uuid.New()

	rooms := []domain.Room{
		{ID: r101ID, PropertyID: propID, FloorId: floor1ID, RoomNumber: "101", Capacity: 2},
		{ID: r102ID, PropertyID: propID, FloorId: floor1ID, RoomNumber: "102", Capacity: 2},
		{ID: r103ID, PropertyID: propID, FloorId: floor1ID, RoomNumber: "103", Capacity: 1}, // Vacant
		{ID: r201ID, PropertyID: propID, FloorId: floor2ID, RoomNumber: "201", Capacity: 3},
		{ID: r202ID, PropertyID: propID, FloorId: floor2ID, RoomNumber: "202", Capacity: 2}, // Vacant
	}

	r101Num := "101"
	r102Num := "102"
	r201Num := "201"

	tenants := map[uuid.UUID]*domain.Tenant{
		uuid.New(): {ID: uuid.New(), PropertyID: propID, Name: "Alice", RoomID: &r101ID, RoomNumber: &r101Num, Status: domain.TenantStatusActive},
		uuid.New(): {ID: uuid.New(), PropertyID: propID, Name: "Bob", RoomID: &r101ID, RoomNumber: &r101Num, Status: domain.TenantStatusActive},
		uuid.New(): {ID: uuid.New(), PropertyID: propID, Name: "Charlie", RoomID: &r102ID, RoomNumber: &r102Num, Status: domain.TenantStatusActive},
		uuid.New(): {ID: uuid.New(), PropertyID: propID, Name: "David", RoomID: &r201ID, RoomNumber: &r201Num, Status: domain.TenantStatusActive},
		uuid.New(): {ID: uuid.New(), PropertyID: propID, Name: "Eva", RoomID: &r201ID, RoomNumber: &r201Num, Status: domain.TenantStatusActive},
	}

	h := &Handlers{
		Deps: Deps{
			JWTSecret:         jwtSecret,
			TenantStore:       &dashboardTenantStore{tenants: tenants},
			GamificationStore: &dashboardGamificationStore{floors: floors, rooms: rooms},
		},
	}

	r := gin.New()
	ownerGroup := r.Group("/owner", auth.RequireOwner(jwtSecret, nil))
	ownerGroup.GET("/occupancy", h.OwnerOccupancy)

	req := httptest.NewRequest(http.MethodGet, "/owner/occupancy", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var res struct {
		PropertyID       uuid.UUID        `json:"property_id"`
		TotalRooms       int              `json:"total_rooms"`
		OccupiedRooms    int              `json:"occupied_rooms"`
		VacantRooms      int              `json:"vacant_rooms"`
		CapacityBeds     int              `json:"capacity_beds"`
		OccupiedBeds     int              `json:"occupied_beds"`
		VacantBeds       int              `json:"vacant_beds"`
		OccupancyRatePct float64          `json:"occupancy_rate_pct"`
		Floors           []FloorOccupancy `json:"floors"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	// 1. Property-wide tie-out checks
	if res.TotalRooms != 5 {
		t.Errorf("expected 5 total rooms, got %d", res.TotalRooms)
	}
	if res.OccupiedRooms != 3 {
		t.Errorf("expected 3 occupied rooms (101, 102, 201), got %d", res.OccupiedRooms)
	}
	if res.VacantRooms != 2 {
		t.Errorf("expected exactly 2 vacant rooms (103, 202), got %d", res.VacantRooms)
	}
	if res.TotalRooms != res.OccupiedRooms+res.VacantRooms {
		t.Errorf("rooms tie-out failed: total(%d) != occupied(%d) + vacant(%d)", res.TotalRooms, res.OccupiedRooms, res.VacantRooms)
	}
	if res.CapacityBeds != 10 {
		t.Errorf("expected 10 total capacity beds, got %d", res.CapacityBeds)
	}
	if res.OccupiedBeds != 5 {
		t.Errorf("expected 5 occupied beds, got %d", res.OccupiedBeds)
	}
	if res.VacantBeds != 5 {
		t.Errorf("expected 5 vacant beds, got %d", res.VacantBeds)
	}
	if res.OccupancyRatePct != 50.0 {
		t.Errorf("expected 50%% occupancy, got %.2f%%", res.OccupancyRatePct)
	}

	// 2. Floor-wise checks
	if len(res.Floors) != 2 {
		t.Fatalf("expected 2 floors, got %d", len(res.Floors))
	}

	// Floor 1 (Ground Floor)
	f1 := res.Floors[0]
	if f1.FloorNumber != 1 || f1.FloorName != "Ground Floor" {
		t.Errorf("unexpected floor 1: %+v", f1)
	}
	if f1.TotalRooms != 3 || f1.OccupiedRooms != 2 || f1.VacantRooms != 1 {
		t.Errorf("floor 1 room counts mismatch: total=%d, occ=%d, vac=%d (expected 3, 2, 1)", f1.TotalRooms, f1.OccupiedRooms, f1.VacantRooms)
	}
	if f1.CapacityBeds != 5 || f1.OccupiedBeds != 3 || f1.VacantBeds != 2 {
		t.Errorf("floor 1 bed counts mismatch: cap=%d, occ=%d, vac=%d (expected 5, 3, 2)", f1.CapacityBeds, f1.OccupiedBeds, f1.VacantBeds)
	}
	if f1.OccupancyRatePct != 60.0 {
		t.Errorf("floor 1 expected 60%% occupancy, got %.2f%%", f1.OccupancyRatePct)
	}
	// Verify room 103 is vacant
	var found103Vacant bool
	for _, rm := range f1.Rooms {
		if rm.RoomNumber == "103" && rm.IsVacant && rm.VacantBeds == 1 {
			found103Vacant = true
		}
	}
	if !found103Vacant {
		t.Errorf("expected room 103 on floor 1 to be vacant with 1 vacant bed")
	}

	// Floor 2 (First Floor)
	f2 := res.Floors[1]
	if f2.FloorNumber != 2 || f2.FloorName != "First Floor" {
		t.Errorf("unexpected floor 2: %+v", f2)
	}
	if f2.TotalRooms != 2 || f2.OccupiedRooms != 1 || f2.VacantRooms != 1 {
		t.Errorf("floor 2 room counts mismatch: total=%d, occ=%d, vac=%d (expected 2, 1, 1)", f2.TotalRooms, f2.OccupiedRooms, f2.VacantRooms)
	}
	if f2.CapacityBeds != 5 || f2.OccupiedBeds != 2 || f2.VacantBeds != 3 {
		t.Errorf("floor 2 bed counts mismatch: cap=%d, occ=%d, vac=%d (expected 5, 2, 3)", f2.CapacityBeds, f2.OccupiedBeds, f2.VacantBeds)
	}
	if f2.OccupancyRatePct != 40.0 {
		t.Errorf("floor 2 expected 40%% occupancy, got %.2f%%", f2.OccupancyRatePct)
	}
	// Verify room 202 is vacant
	var found202Vacant bool
	for _, rm := range f2.Rooms {
		if rm.RoomNumber == "202" && rm.IsVacant && rm.VacantBeds == 2 {
			found202Vacant = true
		}
	}
	if !found202Vacant {
		t.Errorf("expected room 202 on floor 2 to be vacant with 2 vacant beds")
	}
}

func TestBulkMarkCashPaid_PreviewFiltersEligible(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test_secret_bulk_preview"
	propID := uuid.New()
	userID := uuid.New()

	ownerUser := &domain.User{ID: userID, PropertyID: &propID, Role: domain.RoleOwner}
	token, _ := auth.IssueToken(jwtSecret, ownerUser)

	due1 := uuid.New()
	due2 := uuid.New()
	due3 := uuid.New()
	tenant1 := uuid.New()

	dues := map[uuid.UUID]*domain.Due{
		due1: {ID: due1, PropertyID: propID, TenantID: tenant1, Amount: 100000, Status: domain.DueStatusPending, DueDate: time.Now()},
		due2: {ID: due2, PropertyID: propID, TenantID: tenant1, Amount: 50000, Status: domain.DueStatusPartial, DueDate: time.Now()},
		due3: {ID: due3, PropertyID: propID, TenantID: tenant1, Amount: 75000, Status: domain.DueStatusPaid, DueDate: time.Now()}, // Already paid
	}

	h := &Handlers{
		Deps: Deps{
			JWTSecret:   jwtSecret,
			DueStore:    &dashboardDueStore{dues: dues},
			TenantStore: &dashboardTenantStore{tenants: map[uuid.UUID]*domain.Tenant{tenant1: {ID: tenant1, Name: "Test Tenant"}}},
		},
	}

	r := gin.New()
	ownerGroup := r.Group("/owner", auth.RequireOwner(jwtSecret, nil))
	ownerGroup.POST("/dues/bulk-mark-paid/preview", h.BulkMarkCashPaidPreview)

	body := fmt.Sprintf(`{"due_ids":["%s", "%s", "%s"]}`, due1, due2, due3)
	req := httptest.NewRequest(http.MethodPost, "/owner/dues/bulk-mark-paid/preview", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var res struct {
		EligibleCount    int   `json:"eligible_count"`
		TotalAmountPaise int64 `json:"total_amount_paise"`
		SkippedDues      []struct {
			DueID  string `json:"due_id"`
			Reason string `json:"reason"`
		} `json:"skipped_dues"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if res.EligibleCount != 2 {
		t.Errorf("expected 2 eligible dues, got %d", res.EligibleCount)
	}
	if res.TotalAmountPaise != 150000 {
		t.Errorf("expected 150000 paise (100000+50000), got %d", res.TotalAmountPaise)
	}
	if len(res.SkippedDues) != 1 || res.SkippedDues[0].Reason != "paid" {
		t.Errorf("expected 1 skipped paid due, got %+v", res.SkippedDues)
	}
}

func TestBulkMarkCashPaid_ConfirmRejectsMismatchedTotal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test_secret_bulk_confirm_err"
	propID := uuid.New()
	userID := uuid.New()

	ownerUser := &domain.User{ID: userID, PropertyID: &propID, Role: domain.RoleOwner}
	token, _ := auth.IssueToken(jwtSecret, ownerUser)

	dueID := uuid.New()
	dues := map[uuid.UUID]*domain.Due{
		dueID: {ID: dueID, PropertyID: propID, Amount: 100000, Status: domain.DueStatusPending},
	}

	h := &Handlers{
		Deps: Deps{
			JWTSecret: jwtSecret,
			DueStore:  &dashboardDueStore{dues: dues},
		},
	}

	r := gin.New()
	ownerGroup := r.Group("/owner", auth.RequireOwner(jwtSecret, nil))
	ownerGroup.POST("/dues/bulk-mark-paid/confirm", h.BulkMarkCashPaidConfirm)

	// User types 90,000 instead of 100,000 -> mismatch protection must trigger
	body := fmt.Sprintf(`{"due_ids":["%s"], "confirmed_total_paise": 90000}`, dueID)
	req := httptest.NewRequest(http.MethodPost, "/owner/dues/bulk-mark-paid/confirm", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 Bad Request for mismatched total, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "total amount mismatch") {
		t.Errorf("expected mismatch error message, got %s", w.Body.String())
	}
}

func TestBulkMarkCashPaid_ConfirmAppliesPayments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test_secret_bulk_confirm_ok"
	propID := uuid.New()
	userID := uuid.New()

	ownerUser := &domain.User{ID: userID, PropertyID: &propID, Role: domain.RoleOwner}
	token, _ := auth.IssueToken(jwtSecret, ownerUser)

	due1 := uuid.New()
	due2 := uuid.New()
	dues := map[uuid.UUID]*domain.Due{
		due1: {ID: due1, PropertyID: propID, Amount: 50000, Status: domain.DueStatusPending},
		due2: {ID: due2, PropertyID: propID, Amount: 75000, Status: domain.DueStatusPending},
	}

	var paidCount int
	// Override MarkCashPaid
	payStub := &testMarkCashPaidService{
		onMarkPaid: func(dueID uuid.UUID, amt int64, uid uuid.UUID, note string) (*domain.Payment, error) {
			paidCount++
			return &domain.Payment{ID: uuid.New(), DueID: dueID, Amount: amt, MatchedBy: domain.MatchedByCash}, nil
		},
	}

	h := &Handlers{
		Deps: Deps{
			JWTSecret: jwtSecret,
			DueStore:  &dashboardDueStore{dues: dues},
			Payments:  payStub,
		},
	}

	r := gin.New()
	ownerGroup := r.Group("/owner", auth.RequireOwner(jwtSecret, nil))
	ownerGroup.POST("/dues/bulk-mark-paid/confirm", h.BulkMarkCashPaidConfirm)

	body := fmt.Sprintf(`{"due_ids":["%s", "%s"], "confirmed_total_paise": 125000, "note": "Cash handover"}`, due1, due2)
	req := httptest.NewRequest(http.MethodPost, "/owner/dues/bulk-mark-paid/confirm", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
	}

	var res struct {
		BatchRef          string `json:"batch_ref"`
		SettledCount      int    `json:"settled_count"`
		TotalSettledPaise int64  `json:"total_settled_paise"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if res.SettledCount != 2 || paidCount != 2 {
		t.Errorf("expected 2 settled dues, got %d (paidCount: %d)", res.SettledCount, paidCount)
	}
	if res.TotalSettledPaise != 125000 {
		t.Errorf("expected total 125000 paise, got %d", res.TotalSettledPaise)
	}
	if !strings.HasPrefix(res.BatchRef, "bulk_cash_") {
		t.Errorf("expected batch ref starting with bulk_cash_, got %s", res.BatchRef)
	}
}

type testMarkCashPaidService struct {
	*sandboxPaymentService
	onMarkPaid func(dueID uuid.UUID, amt int64, uid uuid.UUID, note string) (*domain.Payment, error)
}

func (s *testMarkCashPaidService) MarkCashPaid(_ context.Context, dueID uuid.UUID, amt int64, uid uuid.UUID, note string) (*domain.Payment, error) {
	if s.onMarkPaid != nil {
		return s.onMarkPaid(dueID, amt, uid, note)
	}
	return &domain.Payment{DueID: dueID, Amount: amt}, nil
}

func TestOwnerCalendar_TokenAndFeedGeneration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "calendar_test_secret_123"
	propID := uuid.New()
	userID := uuid.New()

	ownerUser := &domain.User{ID: userID, PropertyID: &propID, Role: domain.RoleOwner}
	token, _ := auth.IssueToken(jwtSecret, ownerUser)

	dueID1 := uuid.New()
	dueID2 := uuid.New()
	dueDate := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

	dues := map[uuid.UUID]*domain.Due{
		dueID1: {ID: dueID1, PropertyID: propID, TenantID: uuid.New(), Amount: 1500000, Status: domain.DueStatusPending, DueDate: dueDate},
		dueID2: {ID: dueID2, PropertyID: propID, TenantID: uuid.New(), Amount: 1200000, Status: domain.DueStatusPending, DueDate: dueDate},
	}
	props := map[uuid.UUID]*domain.Property{
		propID: {ID: propID, Name: "Green View Residency"},
	}

	h := &Handlers{
		Deps: Deps{
			JWTSecret:        jwtSecret,
			MagicLinkBaseURL: "https://pay.pgcashflow.com",
			PropertyStore:    &dashboardPropStore{props: props},
			DueStore:         &dashboardDueStore{dues: dues},
		},
	}

	r := gin.New()
	ownerGroup := r.Group("/owner", auth.RequireOwner(jwtSecret, nil))
	ownerGroup.GET("/calendar/token", h.OwnerGetCalendarToken)
	r.GET("/api/owner/calendar.ics", h.OwnerCalendarICS)

	// 1. Get calendar subscription URL
	reqToken := httptest.NewRequest(http.MethodGet, "/owner/calendar/token", nil)
	reqToken.Header.Set("Authorization", "Bearer "+token)
	wToken := httptest.NewRecorder()
	r.ServeHTTP(wToken, reqToken)

	if wToken.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for calendar token, got %d: %s", wToken.Code, wToken.Body.String())
	}

	var tokenRes struct {
		Token       string `json:"token"`
		CalendarURL string `json:"calendar_url"`
	}
	_ = json.Unmarshal(wToken.Body.Bytes(), &tokenRes)

	if tokenRes.Token == "" || !strings.Contains(tokenRes.CalendarURL, "/api/owner/calendar.ics?token=") {
		t.Fatalf("unexpected token response: %+v", tokenRes)
	}

	// 2. Access feed without token -> 401 Unauthorized
	reqNoTok := httptest.NewRequest(http.MethodGet, "/api/owner/calendar.ics", nil)
	wNoTok := httptest.NewRecorder()
	r.ServeHTTP(wNoTok, reqNoTok)
	if wNoTok.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for missing calendar token, got %d", wNoTok.Code)
	}

	// 3. Access feed with tampered token -> 401 Unauthorized
	reqBadTok := httptest.NewRequest(http.MethodGet, "/api/owner/calendar.ics?token=bad.tampered.token", nil)
	wBadTok := httptest.NewRecorder()
	r.ServeHTTP(wBadTok, reqBadTok)
	if wBadTok.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for invalid calendar token, got %d", wBadTok.Code)
	}

	// 4. Access feed with valid token -> 200 OK text/calendar
	reqFeed := httptest.NewRequest(http.MethodGet, "/api/owner/calendar.ics?token="+tokenRes.Token, nil)
	wFeed := httptest.NewRecorder()
	r.ServeHTTP(wFeed, reqFeed)

	if wFeed.Code != http.StatusOK {
		t.Fatalf("expected 200 OK for calendar feed, got %d: %s", wFeed.Code, wFeed.Body.String())
	}

	contentType := wFeed.Header().Get("Content-Type")
	if !strings.Contains(contentType, "text/calendar") {
		t.Errorf("expected text/calendar content type, got %s", contentType)
	}

	feedBody := wFeed.Body.String()
	if !strings.Contains(feedBody, "BEGIN:VCALENDAR") || !strings.Contains(feedBody, "END:VCALENDAR") {
		t.Errorf("malformed iCalendar body: %s", feedBody)
	}
	if !strings.Contains(feedBody, "DTSTART;VALUE=DATE:20261005") {
		t.Errorf("missing event date in calendar: %s", feedBody)
	}
	if !strings.Contains(feedBody, "SUMMARY:Rent Due: 2 dues (₹27000)") {
		t.Errorf("expected aggregated summary (2 dues totaling ₹27,000), got:\n%s", feedBody)
	}

	// Verify privacy/anonymity: no tenant sensitive info in calendar
	if strings.Contains(feedBody, "tenant") || strings.Contains(feedBody, "phone") {
		t.Errorf("feed leaked sensitive personal data: %s", feedBody)
	}
}

func TestOwnerPropertySettings_GetAndPatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test_secret_for_settings"
	propID := uuid.New()
	userID := uuid.New()

	ownerUser := &domain.User{
		ID:         userID,
		PropertyID: &propID,
		Role:       domain.RoleOwner,
	}
	token, err := auth.IssueToken(jwtSecret, ownerUser)
	if err != nil {
		t.Fatalf("IssueToken failed: %v", err)
	}

	propStore := &dashboardPropStore{
		props: map[uuid.UUID]*domain.Property{
			propID: {
				ID:   propID,
				Name: "Settings Test PG",
			},
		},
	}

	d := Deps{
		PropertyStore: propStore,
		JWTSecret:     jwtSecret,
	}
	r := NewRouter(d)

	// 1. GET /api/owner/settings -> returns default settings
	reqGet := httptest.NewRequest(http.MethodGet, "/api/owner/settings", nil)
	reqGet.Header.Set("Authorization", "Bearer "+token)
	wGet := httptest.NewRecorder()
	r.ServeHTTP(wGet, reqGet)

	if wGet.Code != http.StatusOK {
		t.Fatalf("expected 200 OK getting settings, got %d: %s", wGet.Code, wGet.Body.String())
	}
	var getRes struct {
		Settings domain.PropertySettings `json:"settings"`
	}
	if err := json.Unmarshal(wGet.Body.Bytes(), &getRes); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	if getRes.Settings.PayoutAutoDispatch != false {
		t.Errorf("expected default PayoutAutoDispatch to be false, got true")
	}
	if getRes.Settings.ReminderCatchUpDays != 2 {
		t.Errorf("expected default ReminderCatchUpDays to be 2, got %d", getRes.Settings.ReminderCatchUpDays)
	}

	// 2. PATCH /api/owner/settings -> update settings
	patchBody := `{"payout_auto_dispatch": true, "reminder_catch_up_days": 4}`
	reqPatch := httptest.NewRequest(http.MethodPatch, "/api/owner/settings", strings.NewReader(patchBody))
	reqPatch.Header.Set("Authorization", "Bearer "+token)
	reqPatch.Header.Set("Content-Type", "application/json")
	wPatch := httptest.NewRecorder()
	r.ServeHTTP(wPatch, reqPatch)

	if wPatch.Code != http.StatusOK {
		t.Fatalf("expected 200 OK updating settings, got %d: %s", wPatch.Code, wPatch.Body.String())
	}
	var patchRes struct {
		Settings domain.PropertySettings `json:"settings"`
	}
	if err := json.Unmarshal(wPatch.Body.Bytes(), &patchRes); err != nil {
		t.Fatalf("unmarshal patch settings: %v", err)
	}
	if patchRes.Settings.PayoutAutoDispatch != true {
		t.Errorf("expected updated PayoutAutoDispatch to be true, got false")
	}
	if patchRes.Settings.ReminderCatchUpDays != 4 {
		t.Errorf("expected updated ReminderCatchUpDays to be 4, got %d", patchRes.Settings.ReminderCatchUpDays)
	}

	// 3. Subsequent GET /api/owner/settings -> reflects persisted update
	reqGet2 := httptest.NewRequest(http.MethodGet, "/api/owner/settings", nil)
	reqGet2.Header.Set("Authorization", "Bearer "+token)
	wGet2 := httptest.NewRecorder()
	r.ServeHTTP(wGet2, reqGet2)

	if wGet2.Code != http.StatusOK {
		t.Fatalf("expected 200 OK getting updated settings, got %d: %s", wGet2.Code, wGet2.Body.String())
	}
	var getRes2 struct {
		Settings domain.PropertySettings `json:"settings"`
	}
	_ = json.Unmarshal(wGet2.Body.Bytes(), &getRes2)
	if getRes2.Settings.PayoutAutoDispatch != true || getRes2.Settings.ReminderCatchUpDays != 4 {
		t.Errorf("persisted settings mismatch: %+v", getRes2.Settings)
	}
}

func TestOwnerDashboardSummaryAndMonthlyCashFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	propID := uuid.New()
	jwtSecret := "test-secret-key-32-bytes-long!!"
	ownerID := uuid.New()
	ownerUser := &domain.User{ID: ownerID, PropertyID: &propID, Role: domain.RoleOwner}
	token, _ := auth.IssueToken(jwtSecret, ownerUser)

	propStore := &dashboardPropStore{
		props: map[uuid.UUID]*domain.Property{
			propID: {ID: propID, Name: "Test PG"},
		},
	}
	tenantStore := &dashboardTenantStore{
		tenants: map[uuid.UUID]*domain.Tenant{
			uuid.New(): {ID: uuid.New(), PropertyID: propID, Status: domain.TenantStatusActive},
		},
	}

	d := Deps{
		PropertyStore: propStore,
		TenantStore:   tenantStore,
		JWTSecret:     jwtSecret,
	}
	r := NewRouter(d)

	t.Run("GET /api/owner/dashboard/summary returns single aggregated payload with integer paise", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/owner/dashboard/summary", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}

		var res OwnerDashboardSummaryResponse
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("unmarshal response failed: %v", err)
		}

		if res.PropertyID != propID {
			t.Errorf("expected property_id %v, got %v", propID, res.PropertyID)
		}
		if res.Period == "" {
			t.Errorf("expected non-empty period")
		}
		if len(res.MonthlyCashFlow) != 6 {
			t.Errorf("expected 6 months in monthly cash flow, got %d", len(res.MonthlyCashFlow))
		}
	})

	t.Run("GET /api/owner/dashboard/monthly-cashflow returns periodic cash flow breakdown", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/owner/dashboard/monthly-cashflow?months=3", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}

		var res struct {
			PropertyID uuid.UUID             `json:"property_id"`
			CashFlow   []MonthlyCashFlowItem `json:"cash_flow"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("unmarshal response failed: %v", err)
		}

		if res.PropertyID != propID {
			t.Errorf("expected property_id %v, got %v", propID, res.PropertyID)
		}
		if len(res.CashFlow) != 3 {
			t.Errorf("expected 3 months returned, got %d", len(res.CashFlow))
		}
	})
}

