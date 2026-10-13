package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/api"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/gamification"
)

type t4cGamificationStore struct {
	gamification.Store
	inspections []domain.Inspection
	disputes    map[uuid.UUID]string
	resolutions map[uuid.UUID]string
	hazards     []domain.HazardReport
	violations  []domain.Violation
	referrals   []domain.Referral
	rsvps       map[string]*domain.MealRSVP // key: tenantID+date+slot
	rsvpsByDate map[string][]domain.MealRSVP
}

func newT4cGamificationStore() *t4cGamificationStore {
	return &t4cGamificationStore{
		disputes:    make(map[uuid.UUID]string),
		resolutions: make(map[uuid.UUID]string),
		rsvps:       make(map[string]*domain.MealRSVP),
		rsvpsByDate: make(map[string][]domain.MealRSVP),
	}
}

func (s *t4cGamificationStore) BeginTx(_ context.Context) (pgx.Tx, error) {
	return nil, nil
}

func (s *t4cGamificationStore) GetSettings(_ context.Context, propID uuid.UUID) (*domain.PropertyGamificationSettings, error) {
	return &domain.PropertyGamificationSettings{
		PropertyID:          propID,
		PointValuePaise:     100,
		EarnCapPerTenant:    200,
		RSVPSubCap:          60,
		FloorBonusThreshold: 85,
	}, nil
}

func (s *t4cGamificationStore) ListInspections(_ context.Context, propertyID uuid.UUID, roomID *uuid.UUID, _ *uuid.UUID, _ int) ([]domain.Inspection, error) {
	var out []domain.Inspection
	for _, insp := range s.inspections {
		if insp.PropertyID == propertyID {
			if roomID == nil || (insp.RoomID != nil && *insp.RoomID == *roomID) {
				out = append(out, insp)
			}
		}
	}
	return out, nil
}

func (s *t4cGamificationStore) GetInspectionByID(_ context.Context, id uuid.UUID) (*domain.Inspection, error) {
	for _, insp := range s.inspections {
		if insp.ID == id {
			return &insp, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (s *t4cGamificationStore) DisputeInspectionItem(_ context.Context, itemID uuid.UUID, disputeNote string) error {
	s.disputes[itemID] = disputeNote
	return nil
}

func (s *t4cGamificationStore) ResolveInspectionItem(_ context.Context, _ uuid.UUID, itemID uuid.UUID, _ uuid.UUID, status string) error {
	s.resolutions[itemID] = status
	return nil
}

func (s *t4cGamificationStore) GetTenantMealRSVP(_ context.Context, tenantID uuid.UUID, date time.Time, slot string) (*domain.MealRSVP, error) {
	key := fmt.Sprintf("%s:%s:%s", tenantID, date.Format("2006-01-02"), slot)
	if r, ok := s.rsvps[key]; ok {
		return r, nil
	}
	return nil, domain.ErrNotFound
}

func (s *t4cGamificationStore) UpsertMealRSVP(_ context.Context, rsvp *domain.MealRSVP) error {
	key := fmt.Sprintf("%s:%s:%s", rsvp.TenantID, rsvp.MealDate.Format("2006-01-02"), rsvp.MealSlot)
	s.rsvps[key] = rsvp
	dateKey := rsvp.MealDate.Format("2006-01-02")
	s.rsvpsByDate[dateKey] = append(s.rsvpsByDate[dateKey], *rsvp)
	return nil
}

func (s *t4cGamificationStore) ListMealRSVPsByDate(_ context.Context, _ uuid.UUID, date time.Time) ([]domain.MealRSVP, error) {
	dateKey := date.Format("2006-01-02")
	return s.rsvpsByDate[dateKey], nil
}

func (s *t4cGamificationStore) CreateHazard(_ context.Context, h *domain.HazardReport) error {
	if h.ID == uuid.Nil {
		h.ID = uuid.New()
	}
	s.hazards = append(s.hazards, *h)
	return nil
}

func (s *t4cGamificationStore) ListViolationsByTenant(_ context.Context, tenantID uuid.UUID) ([]domain.Violation, error) {
	var out []domain.Violation
	for _, v := range s.violations {
		if v.TenantID == tenantID {
			out = append(out, v)
		}
	}
	return out, nil
}

func (s *t4cGamificationStore) ListReferralsByTenant(_ context.Context, tenantID uuid.UUID) ([]domain.Referral, error) {
	var out []domain.Referral
	for _, r := range s.referrals {
		if r.ReferrerTenantID == tenantID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *t4cGamificationStore) CreateReferral(_ context.Context, ref *domain.Referral) error {
	if ref.ID == uuid.Nil {
		ref.ID = uuid.New()
	}
	s.referrals = append(s.referrals, *ref)
	return nil
}

type t4cTenantRepo struct {
	tenants map[uuid.UUID]*domain.Tenant
}

func (r *t4cTenantRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
	if t, ok := r.tenants[id]; ok {
		cp := *t
		return &cp, nil
	}
	return nil, domain.ErrNotFound
}

func (r *t4cTenantRepo) GetByPhone(_ context.Context, _ string) (*domain.Tenant, error) {
	return nil, domain.ErrNotFound
}

func (r *t4cTenantRepo) Update(_ context.Context, t *domain.Tenant) error {
	r.tenants[t.ID] = t
	return nil
}

type t4cEventsPublisher struct {
	events []domain.Event
}

func (p *t4cEventsPublisher) Publish(_ context.Context, e domain.Event) error {
	p.events = append(p.events, e)
	return nil
}

type t4cOutboxStore struct {
	events []*domain.OutboxEvent
}

func (o *t4cOutboxStore) InsertEvent(_ context.Context, evt *domain.OutboxEvent) error {
	o.events = append(o.events, evt)
	return nil
}

func TestTenantAndManagerSelfServiceRoutes_T4c(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test-secret-key-32-chars-long-t4c-selfservice"

	propID := uuid.New()
	roomID := uuid.New()
	tenantUserID := uuid.New()
	managerUserID := uuid.New()
	tenantID := uuid.New()

	tenantModel := &domain.Tenant{
		ID:                     tenantID,
		PropertyID:             propID,
		RoomID:                 &roomID,
		Status:                 domain.TenantStatusActive,
		IsGamificationDisabled: true,
	}

	tenantUser := &domain.User{
		ID:         tenantUserID,
		Role:       domain.RoleTenant,
		TenantID:   &tenantID,
		PropertyID: &propID,
	}

	managerUser := &domain.User{
		ID:         managerUserID,
		Role:       domain.RoleManager,
		PropertyID: &propID,
	}

	tenantToken, err := auth.IssueToken(jwtSecret, tenantUser)
	if err != nil {
		t.Fatalf("issue tenant token: %v", err)
	}

	managerToken, err := auth.IssueToken(jwtSecret, managerUser)
	if err != nil {
		t.Fatalf("issue manager token: %v", err)
	}

	tenantRepo := &t4cTenantRepo{
		tenants: map[uuid.UUID]*domain.Tenant{
			tenantID: tenantModel,
		},
	}

	gamStore := newT4cGamificationStore()
	itemID := uuid.New()
	inspID := uuid.New()

	gamStore.inspections = []domain.Inspection{
		{
			ID:             inspID,
			PropertyID:     propID,
			RoomID:         &roomID,
			InspectionType: "room",
			Passed:         false,
			InspectedAt:    time.Now().UTC().Add(-1 * time.Hour), // within 48h
			Items: []domain.InspectionItem{
				{
					ID:           itemID,
					InspectionID: inspID,
					Description:  "Cleanliness",
					Passed:       false,
				},
			},
		},
	}

	gamStore.violations = []domain.Violation{
		{
			ID:          uuid.New(),
			PropertyID:  propID,
			TenantID:    tenantID,
			RuleCode:    "NOISE_CURFEW",
			Severity:    "lifestyle",
			Description: "Loud music after 11 PM",
		},
	}

	evPub := &t4cEventsPublisher{}
	outbox := &t4cOutboxStore{}
	gamSvc := gamification.NewService(gamStore, tenantRepo, nil, evPub, gamification.NewBlobStore())

	finMem := finance.NewMemoryStore()
	finSvc := finance.NewService(finMem, nil)
	_ = finMem.SaveSettings(context.Background(), domain.PropertyFinanceSettings{
		PropertyID:            propID,
		ManagerCanViewCapital: false,
		ManagerCanViewROI:     false,
	})

	h := &api.Handlers{
		Deps: api.Deps{
			JWTSecret:         jwtSecret,
			AuthTenantRepo:    tenantRepo,
			Gamification:      gamSvc,
			GamificationStore: gamStore,
			Finance:           finSvc,
			FinanceEnabled:    true,
			Events:            evPub,
			OutboxEvents:      outbox,
		},
	}

	router := gin.New()
	apiGroup := router.Group("/api")

	tenantGroup := apiGroup.Group("/tenant", auth.RequireTenant(jwtSecret, tenantRepo, nil))
	{
		tenantGroup.GET("/inspections", h.TenantInspections)
		tenantGroup.POST("/inspections/items/:id/dispute", h.TenantDisputeInspectionItem)
		tenantGroup.GET("/meal-rsvp", h.TenantGetMealRSVP)
		tenantGroup.POST("/meal-rsvp", h.TenantSubmitMealRSVP)
		tenantGroup.POST("/hazards", h.TenantReportHazard)
		tenantGroup.GET("/violations", h.TenantViolations)
		tenantGroup.GET("/referrals", h.TenantReferrals)
		tenantGroup.POST("/referrals", h.TenantReferrals)
	}

	mgrGroup := apiGroup.Group("/manager", auth.RequireManagerOrOwner(jwtSecret, nil), h.ResolveOwnerPropertyScope())
	{
		mgrGroup.POST("/inspections/items/:id/resolve", h.ManagerResolveInspectionItem)
		mgrGroup.GET("/finance/today", h.ManagerFinanceToday)
		mgrGroup.POST("/finance/meal-prep", h.PostMealPrep)
	}

	t.Run("TenantInspections happy path and role boundaries", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/tenant/inspections", nil)
		req.Header.Set("Authorization", "Bearer "+tenantToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		var resp struct {
			Inspections []domain.Inspection `json:"inspections"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if len(resp.Inspections) != 1 || resp.Inspections[0].ID != inspID {
			t.Fatalf("expected inspection %s, got %+v", inspID, resp.Inspections)
		}

		// Manager token cannot access tenant inspections route (403)
		reqMgr := httptest.NewRequest(http.MethodGet, "/api/tenant/inspections", nil)
		reqMgr.Header.Set("Authorization", "Bearer "+managerToken)
		wMgr := httptest.NewRecorder()
		router.ServeHTTP(wMgr, reqMgr)
		if wMgr.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for manager calling tenant route, got %d", wMgr.Code)
		}
	})

	t.Run("TenantDisputeInspectionItem validation and success", func(t *testing.T) {
		// Missing dispute_note -> 400
		badReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/tenant/inspections/items/%s/dispute", itemID), bytes.NewReader([]byte(`{}`)))
		badReq.Header.Set("Authorization", "Bearer "+tenantToken)
		badReq.Header.Set("Content-Type", "application/json")
		badW := httptest.NewRecorder()
		router.ServeHTTP(badW, badReq)
		if badW.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for empty dispute note, got %d body=%s", badW.Code, badW.Body.String())
		}

		// Valid dispute -> 200 OK
		body, _ := json.Marshal(map[string]string{"dispute_note": "Room was cleaned before checkout"})
		goodReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/tenant/inspections/items/%s/dispute", itemID), bytes.NewReader(body))
		goodReq.Header.Set("Authorization", "Bearer "+tenantToken)
		goodReq.Header.Set("Content-Type", "application/json")
		goodW := httptest.NewRecorder()
		router.ServeHTTP(goodW, goodReq)
		if goodW.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", goodW.Code, goodW.Body.String())
		}

		if gamStore.disputes[itemID] != "Room was cleaned before checkout" {
			t.Fatalf("expected dispute note saved, got %q", gamStore.disputes[itemID])
		}

		// Verify outbox event emitted
		foundOutbox := false
		for _, evt := range outbox.events {
			if evt.EventType == string(domain.EvtInspectionDisputed) && evt.PropertyID == propID {
				foundOutbox = true
				break
			}
		}
		if !foundOutbox {
			t.Fatalf("expected EvtInspectionDisputed outbox event")
		}
	})

	t.Run("TenantMealRSVP submit and retrieve", func(t *testing.T) {
		tomorrow := time.Now().UTC().AddDate(0, 0, 1).Format("2006-01-02")

		// Submit RSVP
		body, _ := json.Marshal(map[string]any{
			"date":      tomorrow,
			"slot":      "dinner",
			"attending": true,
		})
		subReq := httptest.NewRequest(http.MethodPost, "/api/tenant/meal-rsvp", bytes.NewReader(body))
		subReq.Header.Set("Authorization", "Bearer "+tenantToken)
		subReq.Header.Set("Content-Type", "application/json")
		subW := httptest.NewRecorder()
		router.ServeHTTP(subW, subReq)
		if subW.Code != http.StatusOK {
			t.Fatalf("expected 200 on submit RSVP, got %d body=%s", subW.Code, subW.Body.String())
		}

		// Submit with invalid date -> 400
		badBody, _ := json.Marshal(map[string]any{
			"date":      "not-a-date",
			"slot":      "dinner",
			"attending": true,
		})
		badReq := httptest.NewRequest(http.MethodPost, "/api/tenant/meal-rsvp", bytes.NewReader(badBody))
		badReq.Header.Set("Authorization", "Bearer "+tenantToken)
		badReq.Header.Set("Content-Type", "application/json")
		badW := httptest.NewRecorder()
		router.ServeHTTP(badW, badReq)
		if badW.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 on malformed date, got %d", badW.Code)
		}

		// Get RSVP for tomorrow
		getReq := httptest.NewRequest(http.MethodGet, "/api/tenant/meal-rsvp", nil)
		getReq.Header.Set("Authorization", "Bearer "+tenantToken)
		getW := httptest.NewRecorder()
		router.ServeHTTP(getW, getReq)
		if getW.Code != http.StatusOK {
			t.Fatalf("expected 200 on get meal RSVP, got %d body=%s", getW.Code, getW.Body.String())
		}
	})

	t.Run("TenantReportHazard submissions", func(t *testing.T) {
		// Missing fields -> 400
		badReq := httptest.NewRequest(http.MethodPost, "/api/tenant/hazards", bytes.NewReader([]byte(`{}`)))
		badReq.Header.Set("Authorization", "Bearer "+tenantToken)
		badReq.Header.Set("Content-Type", "application/json")
		badW := httptest.NewRecorder()
		router.ServeHTTP(badW, badReq)
		if badW.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for empty hazard report, got %d", badW.Code)
		}

		// Valid JSON submission -> 200
		body, _ := json.Marshal(map[string]string{
			"category":    "plumbing",
			"description": "Water leak in floor 2 hallway",
		})
		goodReq := httptest.NewRequest(http.MethodPost, "/api/tenant/hazards", bytes.NewReader(body))
		goodReq.Header.Set("Authorization", "Bearer "+tenantToken)
		goodReq.Header.Set("Content-Type", "application/json")
		goodW := httptest.NewRecorder()
		router.ServeHTTP(goodW, goodReq)
		if goodW.Code != http.StatusOK {
			t.Fatalf("expected 200 on hazard report, got %d body=%s", goodW.Code, goodW.Body.String())
		}

		if len(gamStore.hazards) != 1 || gamStore.hazards[0].Category != "plumbing" {
			t.Fatalf("expected hazard recorded, got %+v", gamStore.hazards)
		}

		// Verify outbox event emitted
		foundOutbox := false
		for _, evt := range outbox.events {
			if evt.EventType == string(domain.EvtHazardReported) && evt.PropertyID == propID {
				foundOutbox = true
				break
			}
		}
		if !foundOutbox {
			t.Fatalf("expected EvtHazardReported outbox event")
		}
	})

	t.Run("TenantViolations listing", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/tenant/violations", nil)
		req.Header.Set("Authorization", "Bearer "+tenantToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}
		var resp struct {
			Violations []domain.Violation `json:"violations"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if len(resp.Violations) != 1 || resp.Violations[0].RuleCode != "NOISE_CURFEW" {
			t.Fatalf("expected violation NOISE_CURFEW, got %+v", resp.Violations)
		}
	})

	t.Run("TenantReferrals get and post", func(t *testing.T) {
		// POST missing phone -> 400
		badReq := httptest.NewRequest(http.MethodPost, "/api/tenant/referrals", bytes.NewReader([]byte(`{"name":"Friend"}`)))
		badReq.Header.Set("Authorization", "Bearer "+tenantToken)
		badReq.Header.Set("Content-Type", "application/json")
		badW := httptest.NewRecorder()
		router.ServeHTTP(badW, badReq)
		if badW.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for missing referral phone, got %d", badW.Code)
		}

		// POST valid referral -> 200
		body, _ := json.Marshal(map[string]string{
			"phone": "+919876543210",
			"name":  "Aarav Sharma",
		})
		postReq := httptest.NewRequest(http.MethodPost, "/api/tenant/referrals", bytes.NewReader(body))
		postReq.Header.Set("Authorization", "Bearer "+tenantToken)
		postReq.Header.Set("Content-Type", "application/json")
		postW := httptest.NewRecorder()
		router.ServeHTTP(postW, postReq)
		if postW.Code != http.StatusOK {
			t.Fatalf("expected 200 on create referral, got %d body=%s", postW.Code, postW.Body.String())
		}

		// GET referrals -> 200
		getReq := httptest.NewRequest(http.MethodGet, "/api/tenant/referrals", nil)
		getReq.Header.Set("Authorization", "Bearer "+tenantToken)
		getW := httptest.NewRecorder()
		router.ServeHTTP(getW, getReq)
		if getW.Code != http.StatusOK {
			t.Fatalf("expected 200 on list referrals, got %d body=%s", getW.Code, getW.Body.String())
		}
		var getResp struct {
			Referrals []domain.Referral `json:"referrals"`
		}
		if err := json.Unmarshal(getW.Body.Bytes(), &getResp); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if len(getResp.Referrals) != 1 || getResp.Referrals[0].Phone != "+919876543210" {
			t.Fatalf("expected 1 referral with phone, got %+v", getResp.Referrals)
		}
	})

	t.Run("ManagerResolveInspectionItem uphold and overturn", func(t *testing.T) {
		// Invalid resolution status -> 400
		badReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/manager/inspections/items/%s/resolve", itemID), bytes.NewReader([]byte(`{"status":"invalid_choice"}`)))
		badReq.Header.Set("Authorization", "Bearer "+managerToken)
		badReq.Header.Set("Content-Type", "application/json")
		badW := httptest.NewRecorder()
		router.ServeHTTP(badW, badReq)
		if badW.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for invalid resolution status, got %d body=%s", badW.Code, badW.Body.String())
		}

		// Valid resolution 'upheld' -> 200
		upheldBody, _ := json.Marshal(map[string]string{"status": "upheld"})
		goodReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/manager/inspections/items/%s/resolve", itemID), bytes.NewReader(upheldBody))
		goodReq.Header.Set("Authorization", "Bearer "+managerToken)
		goodReq.Header.Set("Content-Type", "application/json")
		goodW := httptest.NewRecorder()
		router.ServeHTTP(goodW, goodReq)
		if goodW.Code != http.StatusOK {
			t.Fatalf("expected 200 on resolve item, got %d body=%s", goodW.Code, goodW.Body.String())
		}
		if gamStore.resolutions[itemID] != "upheld" {
			t.Fatalf("expected resolution 'upheld', got %q", gamStore.resolutions[itemID])
		}

		// Tenant cannot access manager resolution route -> 403
		tenReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/manager/inspections/items/%s/resolve", itemID), bytes.NewReader(upheldBody))
		tenReq.Header.Set("Authorization", "Bearer "+tenantToken)
		tenReq.Header.Set("Content-Type", "application/json")
		tenW := httptest.NewRecorder()
		router.ServeHTTP(tenW, tenReq)
		if tenW.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for tenant resolving inspection item, got %d", tenW.Code)
		}
	})

	t.Run("ManagerFinanceToday dashboard", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/manager/finance/today", nil)
		req.Header.Set("Authorization", "Bearer "+managerToken)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 for manager finance today, got %d body=%s", w.Code, w.Body.String())
		}
		var resp struct {
			Expenses         []any `json:"expenses"`
			PendingApprovals []any `json:"pending_approvals"`
			HideCapital      bool  `json:"hide_capital"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("unmarshal error: %v", err)
		}
		if !resp.HideCapital {
			t.Fatalf("expected HideCapital == true for manager without capital permission")
		}

		// Tenant cannot access manager finance today -> 403
		tenReq := httptest.NewRequest(http.MethodGet, "/api/manager/finance/today", nil)
		tenReq.Header.Set("Authorization", "Bearer "+tenantToken)
		tenW := httptest.NewRecorder()
		router.ServeHTTP(tenW, tenReq)
		if tenW.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for tenant accessing manager finance today, got %d", tenW.Code)
		}
	})

	t.Run("PostMealPrep actuals recording", func(t *testing.T) {
		// Invalid JSON -> 400
		badReq := httptest.NewRequest(http.MethodPost, "/api/manager/finance/meal-prep", bytes.NewReader([]byte(`invalid-json`)))
		badReq.Header.Set("Authorization", "Bearer "+managerToken)
		badReq.Header.Set("Content-Type", "application/json")
		badW := httptest.NewRecorder()
		router.ServeHTTP(badW, badReq)
		if badW.Code != http.StatusBadRequest {
			t.Fatalf("expected 400 for malformed json, got %d", badW.Code)
		}

		// Valid meal prep actuals -> 200
		prepBody, _ := json.Marshal(map[string]any{
			"prep_date":         time.Now().Format("2006-01-02"),
			"meal_slot":         "lunch",
			"servings_planned":  50,
			"servings_prepared": 52,
			"total_cost_paise":  520000,
		})
		goodReq := httptest.NewRequest(http.MethodPost, "/api/manager/finance/meal-prep", bytes.NewReader(prepBody))
		goodReq.Header.Set("Authorization", "Bearer "+managerToken)
		goodReq.Header.Set("Content-Type", "application/json")
		goodW := httptest.NewRecorder()
		router.ServeHTTP(goodW, goodReq)
		if goodW.Code != http.StatusOK {
			t.Fatalf("expected 200 for post meal prep, got %d body=%s", goodW.Code, goodW.Body.String())
		}

		// Tenant cannot access manager meal prep -> 403
		tenReq := httptest.NewRequest(http.MethodPost, "/api/manager/finance/meal-prep", bytes.NewReader(prepBody))
		tenReq.Header.Set("Authorization", "Bearer "+tenantToken)
		tenReq.Header.Set("Content-Type", "application/json")
		tenW := httptest.NewRecorder()
		router.ServeHTTP(tenW, tenReq)
		if tenW.Code != http.StatusForbidden {
			t.Fatalf("expected 403 for tenant posting meal prep, got %d", tenW.Code)
		}
	})
}
