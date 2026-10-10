package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
)

// scopeTestPropertyStore is a fixed in-memory PropertyStore.
type scopeTestPropertyStore struct {
	props map[uuid.UUID]*domain.Property
}

func (s *scopeTestPropertyStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Property, error) {
	if p, ok := s.props[id]; ok {
		cp := *p
		return &cp, nil
	}
	return nil, errors.New("property not found")
}
func (s *scopeTestPropertyStore) List(_ context.Context) ([]domain.Property, error) { return nil, nil }
func (s *scopeTestPropertyStore) GetByOwnerPhone(_ context.Context, _ string) (*domain.Property, error) {
	return nil, errors.New("not found")
}
func (s *scopeTestPropertyStore) GetByInviteCode(_ context.Context, _ string) (*domain.Property, error) {
	return nil, errors.New("not found")
}
func (s *scopeTestPropertyStore) Create(_ context.Context, _ *domain.Property) error { return nil }

// scopeTestUserStore is a fixed in-memory UserStore.
type scopeTestUserStore struct {
	users map[uuid.UUID]*domain.User
}

func (s *scopeTestUserStore) Create(_ context.Context, _ *domain.User) error { return nil }
func (s *scopeTestUserStore) GetByPhone(_ context.Context, _ string) (*domain.User, error) {
	return nil, errors.New("not found")
}
func (s *scopeTestUserStore) GetByPropertyAndRole(_ context.Context, _ uuid.UUID, _ domain.Role) ([]domain.User, error) {
	return nil, nil
}
func (s *scopeTestUserStore) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	if u, ok := s.users[id]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, errors.New("user not found")
}
func (s *scopeTestUserStore) SetPropertyID(_ context.Context, _, _ uuid.UUID) error      { return nil }
func (s *scopeTestUserStore) IncrementTokenVersion(_ context.Context, _ uuid.UUID) error { return nil }

// seedForeignExpense inserts one expense that belongs to propertyID and returns its ID.
func seedForeignExpense(t *testing.T, store *finance.MemoryStore, propertyID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	exp := &domain.Expense{
		ID:             id,
		PropertyID:     propertyID,
		IdempotencyKey: "scope-test-" + id.String(),
		AmountPaise:    123400,
	}
	if err := store.InsertExpense(context.Background(), exp); err != nil {
		t.Fatalf("seed expense: %v", err)
	}
	return id
}

// TestManagerRoutes_OwnerWithoutPropertyClaim_CannotReadForeignProperty is the
// regression test for the scope-binding authorization bug. An owner whose token
// carries no property_id must not be able to pick an arbitrary property with the
// X-Property-ID header and read that property's data through a manager route.
func TestManagerRoutes_OwnerWithoutPropertyClaim_CannotReadForeignProperty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "test-secret-key-with-sufficient-length-32"

	victim := uuid.New()
	victimOwnerPhone := "+919000000002"
	attacker := &domain.User{ID: uuid.New(), Role: domain.RoleOwner, Phone: "+919000000001"} // no PropertyID

	memStore := finance.NewMemoryStore()
	victimExpense := seedForeignExpense(t, memStore, victim)

	deps := Deps{
		JWTSecret:      secret,
		Finance:        finance.NewService(memStore, nil),
		FinanceEnabled: true,
		PropertyStore: &scopeTestPropertyStore{props: map[uuid.UUID]*domain.Property{
			victim: {ID: victim, OwnerPhone: victimOwnerPhone},
		}},
		UserStore: &scopeTestUserStore{users: map[uuid.UUID]*domain.User{attacker.ID: attacker}},
	}
	router := NewRouter(deps)

	token, err := auth.IssueToken(secret, attacker)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/manager/finance/expenses", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Property-ID", victim.String())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("owner without property claim read a foreign property via manager route: status %d, want 403 (body: %s)", w.Code, w.Body.String())
	}
	if body := w.Body.String(); strings.Contains(body, victimExpense.String()) {
		t.Fatalf("foreign expense %s leaked in response body: %s", victimExpense, body)
	}
}

// TestManagerRoutes_OwnerOfPropertyViaHeader_StillWorks is the positive
// counterpart: an owner without a property claim who genuinely owns the property
// named by X-Property-ID keeps access, now through the verified resolution path.
func TestManagerRoutes_OwnerOfPropertyViaHeader_StillWorks(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "test-secret-key-with-sufficient-length-32"

	prop := uuid.New()
	owner := &domain.User{ID: uuid.New(), Role: domain.RoleOwner, Phone: "+919000000003"} // no PropertyID

	memStore := finance.NewMemoryStore()
	expID := seedForeignExpense(t, memStore, prop)

	deps := Deps{
		JWTSecret:      secret,
		Finance:        finance.NewService(memStore, nil),
		FinanceEnabled: true,
		PropertyStore: &scopeTestPropertyStore{props: map[uuid.UUID]*domain.Property{
			prop: {ID: prop, OwnerPhone: owner.Phone},
		}},
		UserStore: &scopeTestUserStore{users: map[uuid.UUID]*domain.User{owner.ID: owner}},
	}
	router := NewRouter(deps)

	token, err := auth.IssueToken(secret, owner)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/manager/finance/expenses", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Property-ID", prop.String())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("owner of property denied on manager route: status %d (body: %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), expID.String()) {
		t.Fatalf("expected owned property's expense %s in response, got: %s", expID, w.Body.String())
	}
}
