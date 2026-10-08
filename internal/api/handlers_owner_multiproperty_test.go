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
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type multiPropMemStore struct {
	props map[uuid.UUID]*domain.Property
}

func newMultiPropMemStore() *multiPropMemStore {
	return &multiPropMemStore{props: make(map[uuid.UUID]*domain.Property)}
}

func (m *multiPropMemStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Property, error) {
	p, ok := m.props[id]
	if !ok {
		return nil, postgres.ErrPropertyNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *multiPropMemStore) List(_ context.Context) ([]domain.Property, error) {
	out := make([]domain.Property, 0, len(m.props))
	for _, p := range m.props {
		out = append(out, *p)
	}
	return out, nil
}

func (m *multiPropMemStore) GetByOwnerPhone(_ context.Context, phone string) (*domain.Property, error) {
	for _, p := range m.props {
		if p.OwnerPhone == phone {
			cp := *p
			return &cp, nil
		}
	}
	return nil, postgres.ErrPropertyNotFound
}

func (m *multiPropMemStore) GetByInviteCode(_ context.Context, code string) (*domain.Property, error) {
	for _, p := range m.props {
		if p.InviteCode == code {
			cp := *p
			return &cp, nil
		}
	}
	return nil, postgres.ErrPropertyNotFound
}

func (m *multiPropMemStore) Create(_ context.Context, p *domain.Property) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	p.CreatedAt = time.Now().UTC()
	cp := *p
	m.props[p.ID] = &cp
	return nil
}

type multiPropUserStore struct {
	users map[uuid.UUID]*domain.User
}

func newMultiPropUserStore() *multiPropUserStore {
	return &multiPropUserStore{users: make(map[uuid.UUID]*domain.User)}
}

func (m *multiPropUserStore) Create(_ context.Context, u *domain.User) error {
	m.users[u.ID] = u
	return nil
}

var errTestUserNotFound = errors.New("user not found")

func (m *multiPropUserStore) GetByPhone(_ context.Context, phone string) (*domain.User, error) {
	for _, u := range m.users {
		if u.Phone == phone {
			return u, nil
		}
	}
	return nil, errTestUserNotFound
}

func (m *multiPropUserStore) GetByPropertyAndRole(_ context.Context, propID uuid.UUID, role domain.Role) ([]domain.User, error) {
	var out []domain.User
	for _, u := range m.users {
		if u.PropertyID != nil && *u.PropertyID == propID && u.Role == role {
			out = append(out, *u)
		}
	}
	return out, nil
}

func (m *multiPropUserStore) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	u, ok := m.users[id]
	if !ok {
		return nil, errTestUserNotFound
	}
	return u, nil
}

func (m *multiPropUserStore) SetPropertyID(_ context.Context, userID, propertyID uuid.UUID) error {
	u, ok := m.users[userID]
	if !ok {
		return errTestUserNotFound
	}
	u.PropertyID = &propertyID
	return nil
}

func (m *multiPropUserStore) IncrementTokenVersion(_ context.Context, id uuid.UUID) error {
	u, ok := m.users[id]
	if !ok {
		return errTestUserNotFound
	}
	u.TokenVersion++
	return nil
}

func setupMultiPropRouter(t *testing.T) (*gin.Engine, *Handlers, *domain.User, uuid.UUID, uuid.UUID, uuid.UUID) {
	gin.SetMode(gin.TestMode)
	propStore := newMultiPropMemStore()
	userStore := newMultiPropUserStore()

	ownerPhone := "+919876543210"
	ownerEmail := "owner@test.com"

	p1ID := uuid.New()
	prop1 := &domain.Property{
		ID:         p1ID,
		Name:       "Sunrise PG",
		OwnerPhone: ownerPhone,
		OwnerEmail: ownerEmail,
		OwnerName:  "Sun Owner",
	}
	_ = propStore.Create(context.Background(), prop1)

	p2ID := uuid.New()
	prop2 := &domain.Property{
		ID:         p2ID,
		Name:       "Sunset PG",
		OwnerPhone: ownerPhone,
		OwnerEmail: ownerEmail,
		OwnerName:  "Sun Owner",
	}
	_ = propStore.Create(context.Background(), prop2)

	p3ID := uuid.New()
	prop3 := &domain.Property{
		ID:         p3ID,
		Name:       "Other PG",
		OwnerPhone: "+911111111111",
		OwnerEmail: "other@test.com",
		OwnerName:  "Other Owner",
	}
	_ = propStore.Create(context.Background(), prop3)

	userID := uuid.New()
	user := &domain.User{
		ID:           userID,
		Phone:        ownerPhone,
		Email:        ownerEmail,
		Role:         domain.RoleOwner,
		PropertyID:   &p1ID,
		TokenVersion: 1,
	}
	_ = userStore.Create(context.Background(), user)

	jwtSecret := "test-secret-key-multi-property-test-32bytes"

	h := &Handlers{
		Deps: Deps{
			PropertyStore: propStore,
			UserStore:     userStore,
			AuthUserRepo:  userStore,
		},
	}

	r := gin.New()
	owner := r.Group("/owner", auth.RequireOwner(jwtSecret, userStore), h.ResolveOwnerPropertyScope())
	{
		owner.GET("/properties", h.ListProperties)
		owner.POST("/properties", h.CreateProperty)
		owner.POST("/properties/:id/switch", h.OwnerSwitchProperty)
		owner.GET("/test-scope", func(c *gin.Context) {
			pid, ok := propertyIDFromClaims(c)
			if !ok {
				return
			}
			c.JSON(http.StatusOK, gin.H{"property_id": pid})
		})
	}

	return r, h, user, p1ID, p2ID, p3ID
}

func TestOwner_ListMultipleProperties(t *testing.T) {
	jwtSecret := "test-secret-key-multi-property-test-32bytes"
	r, _, user, p1ID, p2ID, p3ID := setupMultiPropRouter(t)

	token, err := auth.IssueToken(jwtSecret, user)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	req := httptest.NewRequest("GET", "/owner/properties", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Properties []domain.Property `json:"properties"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(resp.Properties) != 2 {
		t.Fatalf("expected 2 properties, got %d", len(resp.Properties))
	}

	ids := map[uuid.UUID]bool{}
	for _, p := range resp.Properties {
		ids[p.ID] = true
	}
	if !ids[p1ID] || !ids[p2ID] {
		t.Errorf("expected properties %s and %s, got %v", p1ID, p2ID, ids)
	}
	if ids[p3ID] {
		t.Errorf("unowned property %s should NOT be listed", p3ID)
	}
}

func TestOwner_PropertyScoping_XPropertyIDHeader(t *testing.T) {
	jwtSecret := "test-secret-key-multi-property-test-32bytes"
	r, _, user, p1ID, p2ID, p3ID := setupMultiPropRouter(t)

	token, err := auth.IssueToken(jwtSecret, user)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	// 1. Without header -> defaults to Primary (p1ID)
	reqDefault := httptest.NewRequest("GET", "/owner/test-scope", nil)
	reqDefault.Header.Set("Authorization", "Bearer "+token)
	wDefault := httptest.NewRecorder()
	r.ServeHTTP(wDefault, reqDefault)

	if wDefault.Code != http.StatusOK {
		t.Fatalf("default scope: expected 200, got %d", wDefault.Code)
	}
	var resDefault struct {
		PropertyID uuid.UUID `json:"property_id"`
	}
	_ = json.Unmarshal(wDefault.Body.Bytes(), &resDefault)
	if resDefault.PropertyID != p1ID {
		t.Errorf("default scope: got %s, want %s", resDefault.PropertyID, p1ID)
	}

	// 2. With X-Property-ID pointing to owned p2ID -> switches scope to p2ID
	reqP2 := httptest.NewRequest("GET", "/owner/test-scope", nil)
	reqP2.Header.Set("Authorization", "Bearer "+token)
	reqP2.Header.Set("X-Property-ID", p2ID.String())
	wP2 := httptest.NewRecorder()
	r.ServeHTTP(wP2, reqP2)

	if wP2.Code != http.StatusOK {
		t.Fatalf("p2 scope: expected 200, got %d: %s", wP2.Code, wP2.Body.String())
	}
	var resP2 struct {
		PropertyID uuid.UUID `json:"property_id"`
	}
	_ = json.Unmarshal(wP2.Body.Bytes(), &resP2)
	if resP2.PropertyID != p2ID {
		t.Errorf("p2 scope: got %s, want %s", resP2.PropertyID, p2ID)
	}

	// 3. With X-Property-ID pointing to UNOWNED p3ID -> rejected with 403 Forbidden
	reqP3 := httptest.NewRequest("GET", "/owner/test-scope", nil)
	reqP3.Header.Set("Authorization", "Bearer "+token)
	reqP3.Header.Set("X-Property-ID", p3ID.String())
	wP3 := httptest.NewRecorder()
	r.ServeHTTP(wP3, reqP3)

	if wP3.Code != http.StatusForbidden {
		t.Fatalf("unowned p3 scope: expected 403, got %d: %s", wP3.Code, wP3.Body.String())
	}
}

func TestOwner_SwitchPropertyEndpoint(t *testing.T) {
	jwtSecret := "test-secret-key-multi-property-test-32bytes"
	r, _, user, _, p2ID, p3ID := setupMultiPropRouter(t)

	token, err := auth.IssueToken(jwtSecret, user)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	// Switch to owned property p2
	req := httptest.NewRequest("POST", "/owner/properties/"+p2ID.String()+"/switch", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("switch to p2: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var res struct {
		OK       bool            `json:"ok"`
		Property domain.Property `json:"property"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.Property.ID != p2ID {
		t.Errorf("switch property: got %s, want %s", res.Property.ID, p2ID)
	}

	// Try to switch to unowned property p3 -> 403
	reqBad := httptest.NewRequest("POST", "/owner/properties/"+p3ID.String()+"/switch", nil)
	reqBad.Header.Set("Authorization", "Bearer "+token)
	wBad := httptest.NewRecorder()
	r.ServeHTTP(wBad, reqBad)

	if wBad.Code != http.StatusForbidden {
		t.Fatalf("switch to unowned p3: expected 403, got %d: %s", wBad.Code, wBad.Body.String())
	}
}

func TestOwner_CreateProperty(t *testing.T) {
	jwtSecret := "test-secret-key-multi-property-test-32bytes"
	r, _, user, _, _, _ := setupMultiPropRouter(t)

	token, err := auth.IssueToken(jwtSecret, user)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}

	body := map[string]string{
		"name":       "Horizon PG",
		"address":    "123 MG Road, Bangalore",
		"owner_name": "Sun Owner",
	}
	raw, _ := json.Marshal(body)

	req := httptest.NewRequest("POST", "/owner/properties", bytes.NewReader(raw))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("create property: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var res struct {
		Property domain.Property `json:"property"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if res.Property.Name != "Horizon PG" {
		t.Errorf("created name: got %q, want 'Horizon PG'", res.Property.Name)
	}
	if res.Property.OwnerPhone != user.Phone {
		t.Errorf("created owner_phone: got %q, want %q", res.Property.OwnerPhone, user.Phone)
	}
}
