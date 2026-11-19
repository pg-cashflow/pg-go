package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type inMemoryUserStore struct {
	users map[uuid.UUID]*domain.User
}

func newInMemoryUserStore() *inMemoryUserStore {
	return &inMemoryUserStore{users: make(map[uuid.UUID]*domain.User)}
}

func (s *inMemoryUserStore) Create(_ context.Context, u *domain.User) error {
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	u.CreatedAt = time.Now().UTC()
	s.users[u.ID] = u
	return nil
}

func (s *inMemoryUserStore) GetByPhone(_ context.Context, phone string) (*domain.User, error) {
	for _, u := range s.users {
		if u.Phone == phone {
			return u, nil
		}
	}
	return nil, domain.ErrNotFound
}

func (s *inMemoryUserStore) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	u, ok := s.users[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return u, nil
}

func (s *inMemoryUserStore) GetByPropertyAndRole(_ context.Context, pid uuid.UUID, role domain.Role) ([]domain.User, error) {
	var out []domain.User
	for _, u := range s.users {
		if u.PropertyID != nil && *u.PropertyID == pid && u.Role == role {
			out = append(out, *u)
		}
	}
	return out, nil
}

func (s *inMemoryUserStore) SetPropertyID(_ context.Context, userID, pid uuid.UUID) error {
	if u, ok := s.users[userID]; ok {
		u.PropertyID = &pid
	}
	return nil
}

func (s *inMemoryUserStore) IncrementTokenVersion(_ context.Context, userID uuid.UUID) error {
	if u, ok := s.users[userID]; ok {
		u.TokenVersion++
	}
	return nil
}

type inMemoryCapabilityStore struct {
	grants map[uuid.UUID]map[string]bool
}

func newInMemoryCapabilityStore() *inMemoryCapabilityStore {
	return &inMemoryCapabilityStore{grants: make(map[uuid.UUID]map[string]bool)}
}

func (s *inMemoryCapabilityStore) HasActive(_ context.Context, _, userID uuid.UUID, capability string) (bool, error) {
	userGrants, ok := s.grants[userID]
	if !ok {
		return false, nil
	}
	return userGrants[capability], nil
}

func (s *inMemoryCapabilityStore) ListActive(_ context.Context, _, userID uuid.UUID) ([]string, error) {
	userGrants, ok := s.grants[userID]
	if !ok {
		return []string{}, nil
	}
	var out []string
	for cap, active := range userGrants {
		if active {
			out = append(out, cap)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *inMemoryCapabilityStore) Grant(_ context.Context, _, userID uuid.UUID, capability string, _ uuid.UUID) error {
	if _, ok := s.grants[userID]; !ok {
		s.grants[userID] = make(map[string]bool)
	}
	s.grants[userID][capability] = true
	return nil
}

func (s *inMemoryCapabilityStore) Revoke(_ context.Context, _, userID uuid.UUID, capability string) error {
	if userGrants, ok := s.grants[userID]; ok {
		delete(userGrants, capability)
	}
	return nil
}

func (s *inMemoryCapabilityStore) RevokeAll(_ context.Context, _, userID uuid.UUID) error {
	delete(s.grants, userID)
	return nil
}

func setupManagerTestRouter(userStore *inMemoryUserStore, capStore *inMemoryCapabilityStore, jwtSecret string) (*gin.Engine, *Handlers) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	h := &Handlers{Deps: Deps{
		JWTSecret:       jwtSecret,
		UserStore:       userStore,
		CapabilityStore: capStore,
		AuthUserRepo:    userStore,
	}}

	owner := router.Group("/api/owner", auth.RequireOwner(jwtSecret, nil), h.ResolveOwnerPropertyScope())
	owner.GET("/managers", h.OwnerListManagers)
	owner.POST("/managers", h.OwnerCreateManager)
	owner.GET("/managers/audit", h.OwnerAuditManagers)
	owner.PUT("/managers/:id/capabilities", h.OwnerPutManagerCapabilities)
	owner.POST("/managers/:id/deactivate", h.OwnerDeactivateManager)
	owner.POST("/managers/:id/reactivate", h.OwnerReactivateManager)
	owner.POST("/managers/:id/reset-credentials", h.OwnerResetManagerCredentials)
	owner.POST("/managers/:id/repair", h.OwnerRepairManager)

	return router, h
}

func TestOwnerCreateManager_Profiles(t *testing.T) {
	jwtSecret := "test-secret-key-with-sufficient-length-32"
	propID := uuid.New()
	ownerToken, err := auth.IssueToken(jwtSecret, &domain.User{
		ID:         uuid.New(),
		Role:       domain.RoleOwner,
		PropertyID: &propID,
	})
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	tests := []struct {
		name         string
		payload      map[string]any
		wantStatus   int
		wantProfile  string
		wantCaps     []string
		wantErrMatch string
	}{
		{
			name: "Default profile is front_desk with 3 capabilities",
			payload: map[string]any{
				"phone":       "9876543210",
				"property_id": propID.String(),
			},
			wantStatus:  http.StatusOK,
			wantProfile: ProfileFrontDesk,
			wantCaps: []string{
				"complaints.respond",
				"dues.view",
				"tenants.view",
			},
		},
		{
			name: "Explicit operations profile with 5 capabilities",
			payload: map[string]any{
				"phone":       "9876543211",
				"property_id": propID.String(),
				"profile":     "operations",
			},
			wantStatus:  http.StatusOK,
			wantProfile: ProfileOperations,
			wantCaps: []string{
				"attendance.mark",
				"complaints.respond",
				"inspections.write",
				"kitchen.headcount",
				"meters.write",
			},
		},
		{
			name: "Explicit finance_assistant profile with 4 capabilities",
			payload: map[string]any{
				"phone":       "9876543212",
				"property_id": propID.String(),
				"profile":     "finance_assistant",
			},
			wantStatus:  http.StatusOK,
			wantProfile: ProfileFinanceAssistant,
			wantCaps: []string{
				"dues.view",
				"expenses.post",
				"refunds.request",
				"tenants.view",
			},
		},
		{
			name: "Custom profile with explicit capabilities",
			payload: map[string]any{
				"phone":       "9876543213",
				"property_id": propID.String(),
				"profile":     "custom",
				"capabilities": []string{
					"tenants.view",
					"refunds.request",
				},
			},
			wantStatus:  http.StatusOK,
			wantProfile: ProfileCustom,
			wantCaps: []string{
				"refunds.request",
				"tenants.view",
			},
		},
		{
			name: "Invalid profile rejected with 400",
			payload: map[string]any{
				"phone":       "9876543214",
				"property_id": propID.String(),
				"profile":     "super_admin",
			},
			wantStatus:   http.StatusBadRequest,
			wantErrMatch: "invalid profile",
		},
		{
			name: "Custom profile with non-delegable capability rejected with 400",
			payload: map[string]any{
				"phone":       "9876543215",
				"property_id": propID.String(),
				"profile":     "custom",
				"capabilities": []string{
					"tenants.view",
					"kyc.documents.review", // Non-delegable
				},
			},
			wantStatus:   http.StatusBadRequest,
			wantErrMatch: "capability not delegable",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			userStore := newInMemoryUserStore()
			capStore := newInMemoryCapabilityStore()
			router, _ := setupManagerTestRouter(userStore, capStore, jwtSecret)

			body, _ := json.Marshal(tt.payload)
			req := httptest.NewRequest(http.MethodPost, "/api/owner/managers", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+ownerToken)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			router.ServeHTTP(w, req)

			if w.Code != tt.wantStatus {
				t.Fatalf("expected status %d, got %d body=%s", tt.wantStatus, w.Code, w.Body.String())
			}

			if tt.wantErrMatch != "" {
				if !strings.Contains(w.Body.String(), tt.wantErrMatch) {
					t.Fatalf("expected error containing %q, got %s", tt.wantErrMatch, w.Body.String())
				}
				return
			}

			// Verify created user profile in storage
			var createdUser *domain.User
			for _, u := range userStore.users {
				if u.Phone == auth.NormalizePhone(tt.payload["phone"].(string)) {
					createdUser = u
					break
				}
			}
			if createdUser == nil {
				t.Fatalf("user not found in store")
			}
			if createdUser.Profile != tt.wantProfile {
				t.Fatalf("expected user profile %q, got %q", tt.wantProfile, createdUser.Profile)
			}

			// Verify active capabilities in storage
			caps, err := capStore.ListActive(context.Background(), propID, createdUser.ID)
			if err != nil {
				t.Fatalf("list active caps: %v", err)
			}
			sort.Strings(caps)
			sort.Strings(tt.wantCaps)
			if len(caps) != len(tt.wantCaps) {
				t.Fatalf("expected %d caps (%v), got %d (%v)", len(tt.wantCaps), tt.wantCaps, len(caps), caps)
			}
			for i := range caps {
				if caps[i] != tt.wantCaps[i] {
					t.Errorf("cap %d mismatch: want %q, got %q", i, tt.wantCaps[i], caps[i])
				}
			}
		})
	}
}

func TestOwnerPutManagerCapabilities(t *testing.T) {
	jwtSecret := "test-secret-key-with-sufficient-length-32"
	propID := uuid.New()
	ownerToken, _ := auth.IssueToken(jwtSecret, &domain.User{
		ID:         uuid.New(),
		Role:       domain.RoleOwner,
		PropertyID: &propID,
	})

	userStore := newInMemoryUserStore()
	capStore := newInMemoryCapabilityStore()
	router, _ := setupManagerTestRouter(userStore, capStore, jwtSecret)

	mgrID := uuid.New()
	activeMgr := &domain.User{
		ID:           mgrID,
		Phone:        "+919876543210",
		Role:         domain.RoleManager,
		PropertyID:   &propID,
		Profile:      ProfileFrontDesk,
		TokenVersion: 1,
	}
	userStore.users[mgrID] = activeMgr
	_ = capStore.Grant(context.Background(), propID, mgrID, "tenants.view", uuid.New())
	_ = capStore.Grant(context.Background(), propID, mgrID, "dues.view", uuid.New())

	t.Run("Successfully update capabilities on active manager", func(t *testing.T) {
		payload := map[string]any{
			"grant":  []string{"refunds.request"},
			"revoke": []string{"dues.view"},
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPut, "/api/owner/managers/"+mgrID.String()+"/capabilities", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
		}

		caps, _ := capStore.ListActive(context.Background(), propID, mgrID)
		sort.Strings(caps)
		wantCaps := []string{"refunds.request", "tenants.view"}
		if len(caps) != len(wantCaps) || caps[0] != wantCaps[0] || caps[1] != wantCaps[1] {
			t.Fatalf("expected caps %v, got %v", wantCaps, caps)
		}
		if activeMgr.TokenVersion != 2 {
			t.Fatalf("expected token version incremented to 2, got %d", activeMgr.TokenVersion)
		}
	})

	t.Run("Deactivated manager returns 403 Forbidden", func(t *testing.T) {
		deactID := uuid.New()
		now := time.Now().UTC()
		deactMgr := &domain.User{
			ID:         deactID,
			Phone:      "+919876543219",
			Role:       domain.RoleManager,
			PropertyID: &propID,
			Profile:    ProfileFrontDesk,
			DisabledAt: &now,
		}
		userStore.users[deactID] = deactMgr

		payload := map[string]any{
			"grant": []string{"tenants.view"},
		}
		body, _ := json.Marshal(payload)
		req := httptest.NewRequest(http.MethodPut, "/api/owner/managers/"+deactID.String()+"/capabilities", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("expected 403 Forbidden for deactivated manager, got %d body=%s", w.Code, w.Body.String())
		}
	})
}

func TestOwnerDeactivateAndReactivateManager(t *testing.T) {
	jwtSecret := "test-secret-key-with-sufficient-length-32"
	propID := uuid.New()
	ownerToken, _ := auth.IssueToken(jwtSecret, &domain.User{
		ID:         uuid.New(),
		Role:       domain.RoleOwner,
		PropertyID: &propID,
	})

	userStore := newInMemoryUserStore()
	capStore := newInMemoryCapabilityStore()
	router, _ := setupManagerTestRouter(userStore, capStore, jwtSecret)

	mgrID := uuid.New()
	mgr := &domain.User{
		ID:           mgrID,
		Phone:        "+919876543210",
		Role:         domain.RoleManager,
		PropertyID:   &propID,
		Profile:      ProfileFrontDesk,
		TokenVersion: 1,
	}
	userStore.users[mgrID] = mgr
	_ = capStore.Grant(context.Background(), propID, mgrID, "tenants.view", uuid.New())

	// 1. Deactivate
	req := httptest.NewRequest(http.MethodPost, "/api/owner/managers/"+mgrID.String()+"/deactivate", nil)
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on deactivate, got %d body=%s", w.Code, w.Body.String())
	}
	if !mgr.IsDisabled() {
		t.Fatalf("expected manager to be disabled")
	}
	caps, _ := capStore.ListActive(context.Background(), propID, mgrID)
	if len(caps) != 0 {
		t.Fatalf("expected all capabilities revoked on deactivate, got %v", caps)
	}

	// 2. Reactivate
	req = httptest.NewRequest(http.MethodPost, "/api/owner/managers/"+mgrID.String()+"/reactivate", nil)
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 on reactivate, got %d body=%s", w.Code, w.Body.String())
	}
	if mgr.IsDisabled() {
		t.Fatalf("expected manager to be active after reactivate")
	}
}

func TestOwnerAuditAndRepairManagers(t *testing.T) {
	jwtSecret := "test-secret-key-with-sufficient-length-32"
	propID := uuid.New()
	ownerToken, _ := auth.IssueToken(jwtSecret, &domain.User{
		ID:         uuid.New(),
		Role:       domain.RoleOwner,
		PropertyID: &propID,
	})

	userStore := newInMemoryUserStore()
	capStore := newInMemoryCapabilityStore()
	router, _ := setupManagerTestRouter(userStore, capStore, jwtSecret)

	// Clean Manager: front_desk with exact 3 grants
	cleanID := uuid.New()
	cleanMgr := &domain.User{
		ID:         cleanID,
		Phone:      "+919876543201",
		Role:       domain.RoleManager,
		PropertyID: &propID,
		Profile:    ProfileFrontDesk,
	}
	userStore.users[cleanID] = cleanMgr
	for _, c := range profileCapabilities[ProfileFrontDesk] {
		_ = capStore.Grant(context.Background(), propID, cleanID, c, uuid.New())
	}

	// Drifted Manager: operations profile but missing 2 grants
	driftID := uuid.New()
	driftMgr := &domain.User{
		ID:         driftID,
		Phone:      "+919876543202",
		Role:       domain.RoleManager,
		PropertyID: &propID,
		Profile:    ProfileOperations,
	}
	userStore.users[driftID] = driftMgr
	_ = capStore.Grant(context.Background(), propID, driftID, "attendance.mark", uuid.New())

	// Deactivated Manager with active grant
	deactID := uuid.New()
	now := time.Now().UTC()
	deactMgr := &domain.User{
		ID:         deactID,
		Phone:      "+919876543203",
		Role:       domain.RoleManager,
		PropertyID: &propID,
		Profile:    ProfileFrontDesk,
		DisabledAt: &now,
	}
	userStore.users[deactID] = deactMgr
	_ = capStore.Grant(context.Background(), propID, deactID, "tenants.view", uuid.New())

	// Test GET /api/owner/managers/audit
	req := httptest.NewRequest(http.MethodGet, "/api/owner/managers/audit", nil)
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("audit expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	var summary ManagerAuditSummary
	if err := json.Unmarshal(w.Body.Bytes(), &summary); err != nil {
		t.Fatalf("unmarshal audit summary: %v", err)
	}

	if summary.TotalManagers != 3 {
		t.Fatalf("expected 3 managers, got %d", summary.TotalManagers)
	}
	if summary.CleanCount != 1 {
		t.Errorf("expected 1 clean manager, got %d", summary.CleanCount)
	}
	if summary.DriftCount != 2 {
		t.Errorf("expected 2 drifted managers, got %d", summary.DriftCount)
	}

	// Verify repair for drifted operations manager
	req = httptest.NewRequest(http.MethodPost, "/api/owner/managers/"+driftID.String()+"/repair", nil)
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("repair expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	repairedCaps, _ := capStore.ListActive(context.Background(), propID, driftID)
	wantOpsCaps := profileCapabilities[ProfileOperations]
	sort.Strings(repairedCaps)
	sort.Strings(wantOpsCaps)
	if len(repairedCaps) != len(wantOpsCaps) {
		t.Fatalf("expected %d repaired caps, got %d", len(wantOpsCaps), len(repairedCaps))
	}

	// Verify repair for deactivated manager
	req = httptest.NewRequest(http.MethodPost, "/api/owner/managers/"+deactID.String()+"/repair", nil)
	req.Header.Set("Authorization", "Bearer "+ownerToken)
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("repair deact expected 200, got %d body=%s", w.Code, w.Body.String())
	}

	deactCaps, _ := capStore.ListActive(context.Background(), propID, deactID)
	if len(deactCaps) != 0 {
		t.Fatalf("expected 0 caps after repairing deactivated manager, got %v", deactCaps)
	}
}
