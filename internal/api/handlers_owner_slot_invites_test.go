package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/slotinvite"
)

type stubSlotInviteService struct {
	revoke func(ctx context.Context, propertyID, inviteID, actor uuid.UUID) (*domain.SlotInvite, error)
}

func (s *stubSlotInviteService) Create(context.Context, slotinvite.CreateInput) (*domain.SlotInvite, error) {
	return nil, nil
}

func (s *stubSlotInviteService) List(context.Context, uuid.UUID, *domain.SlotInviteStatus) ([]domain.SlotInvite, error) {
	return nil, nil
}

func (s *stubSlotInviteService) Revoke(ctx context.Context, propertyID, inviteID, actor uuid.UUID) (*domain.SlotInvite, error) {
	if s.revoke != nil {
		return s.revoke(ctx, propertyID, inviteID, actor)
	}
	return nil, postgres.ErrSlotCrossProperty
}

func TestOwnerRevokeSlotInvite_CrossPropertyIDOR(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "test-secret-key-with-sufficient-length-32"
	propA := uuid.New()
	owner := &domain.User{ID: uuid.New(), Role: domain.RoleOwner, Phone: "+919000000201", PropertyID: &propA}
	inviteID := uuid.New()

	router := NewRouter(Deps{
		JWTSecret: secret,
		PropertyStore: &scopeTestPropertyStore{props: map[uuid.UUID]*domain.Property{
			propA: {ID: propA, OwnerPhone: owner.Phone},
		}},
		UserStore:   &scopeTestUserStore{users: map[uuid.UUID]*domain.User{owner.ID: owner}},
		SlotInvites: &stubSlotInviteService{
			revoke: func(_ context.Context, propertyID, id, _ uuid.UUID) (*domain.SlotInvite, error) {
				if propertyID != propA || id != inviteID {
					t.Fatalf("unexpected revoke scope property=%s invite=%s", propertyID, id)
				}
				return nil, postgres.ErrSlotCrossProperty
			},
		},
	})

	tok, err := auth.IssueToken(secret, owner)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.NewBufferString("{}")
	req := httptest.NewRequest(http.MethodPost, "/api/owner/slot-invites/"+inviteID.String()+"/revoke", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Code != string(apierr.CodeJoinNotFound) {
		t.Fatalf("code %q want %s", env.Code, apierr.CodeJoinNotFound)
	}
}

func TestManagerCreateSlotInvite_RequiresCapability(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "test-secret-key-with-sufficient-length-32"
	propA := uuid.New()
	manager := &domain.User{ID: uuid.New(), Role: domain.RoleManager, Phone: "+919000000202", PropertyID: &propA}

	router := NewRouter(Deps{
		JWTSecret: secret,
		PropertyStore: &scopeTestPropertyStore{props: map[uuid.UUID]*domain.Property{
			propA: {ID: propA, OwnerPhone: "+919000000299"},
		}},
		UserStore: &scopeTestUserStore{users: map[uuid.UUID]*domain.User{manager.ID: manager}},
		SlotInvites: &stubSlotInviteService{},
		CapabilityStore: &stubCapabilityStore{active: map[string]bool{}},
	})

	tok, err := auth.IssueToken(secret, manager)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/manager/slot-invites", bytes.NewBufferString(`{"floor_id":"`+uuid.New().String()+`","room_id":"`+uuid.New().String()+`","rent_paise":100,"due_day":5}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("manager without slot_invites.create: status %d body %s", w.Code, w.Body.String())
	}
}

type stubCapabilityStore struct {
	active map[string]bool
}

func (s *stubCapabilityStore) HasActive(_ context.Context, _ uuid.UUID, _ uuid.UUID, capability string) (bool, error) {
	return s.active[capability], nil
}

func (s *stubCapabilityStore) ListActive(context.Context, uuid.UUID, uuid.UUID) ([]string, error) {
	return nil, nil
}

func (s *stubCapabilityStore) Grant(context.Context, uuid.UUID, uuid.UUID, string, uuid.UUID) error {
	return nil
}

func (s *stubCapabilityStore) Revoke(context.Context, uuid.UUID, uuid.UUID, string) error {
	return nil
}

func (s *stubCapabilityStore) RevokeAll(context.Context, uuid.UUID, uuid.UUID) error {
	return nil
}
