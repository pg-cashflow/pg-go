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
	"github.com/pg-cashflow/pg-go/internal/waitlist"
)

type stubWaitlistSvc struct{}

func (stubWaitlistSvc) Join(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ uuid.UUID, _ string) (*domain.RoomWaitlistEntry, error) {
	return nil, postgres.ErrWaitlistDisabled
}
func (stubWaitlistSvc) List(_ context.Context, _ uuid.UUID, _ uuid.UUID) ([]domain.RoomWaitlistEntry, error) {
	return nil, nil
}
func (stubWaitlistSvc) OfferNext(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ uuid.UUID) (*waitlist.OfferResult, error) {
	return nil, nil
}
func (stubWaitlistSvc) AcceptOffer(_ context.Context, _ uuid.UUID, _ string, _ string, _ func(context.Context, uuid.UUID) error) (*domain.SlotInvite, error) {
	return nil, nil
}

type stubWaitlistCrossProperty struct{}

func (stubWaitlistCrossProperty) Join(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ uuid.UUID, _ string) (*domain.RoomWaitlistEntry, error) {
	return nil, postgres.ErrSlotCrossProperty
}
func (stubWaitlistCrossProperty) List(_ context.Context, _ uuid.UUID, _ uuid.UUID) ([]domain.RoomWaitlistEntry, error) {
	return nil, nil
}
func (stubWaitlistCrossProperty) OfferNext(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ uuid.UUID) (*waitlist.OfferResult, error) {
	return nil, nil
}
func (stubWaitlistCrossProperty) AcceptOffer(_ context.Context, _ uuid.UUID, _ string, _ string, _ func(context.Context, uuid.UUID) error) (*domain.SlotInvite, error) {
	return nil, nil
}

type stubWaitlistRoomOpen struct{}

func (stubWaitlistRoomOpen) Join(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ uuid.UUID, _ string) (*domain.RoomWaitlistEntry, error) {
	return nil, postgres.ErrWaitlistRoomOpen
}
func (stubWaitlistRoomOpen) List(_ context.Context, _ uuid.UUID, _ uuid.UUID) ([]domain.RoomWaitlistEntry, error) {
	return nil, nil
}
func (stubWaitlistRoomOpen) OfferNext(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ uuid.UUID) (*waitlist.OfferResult, error) {
	return nil, nil
}
func (stubWaitlistRoomOpen) AcceptOffer(_ context.Context, _ uuid.UUID, _ string, _ string, _ func(context.Context, uuid.UUID) error) (*domain.SlotInvite, error) {
	return nil, nil
}

func TestJoinWaitlist_Disabled_Returns422(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "test-secret-key-with-sufficient-length-32"
	propA := uuid.New()
	pending := &domain.User{ID: uuid.New(), Role: domain.RoleTenant, Phone: "+919000000501", PropertyID: &propA}

	userStore := &scopeTestUserStore{users: map[uuid.UUID]*domain.User{pending.ID: pending}}
	router := NewRouter(Deps{
		JWTSecret: secret,
		PropertyStore: &scopeTestPropertyStore{props: map[uuid.UUID]*domain.Property{
			propA: {ID: propA, OwnerPhone: "+919000000599"},
		}},
		UserStore:    userStore,
		AuthUserRepo: userStore,
		Waitlist:     stubWaitlistSvc{},
	})

	tok, err := auth.IssueToken(secret, pending)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.NewBufferString(`{"room_id":"` + uuid.New().String() + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/join/waitlist", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Code != string(apierr.CodeWaitlistDisabled) {
		t.Fatalf("code %q want %s", env.Code, apierr.CodeWaitlistDisabled)
	}
}

func TestJoinWaitlist_ForeignRoom_Returns404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "test-secret-key-with-sufficient-length-32"
	propA := uuid.New()
	pending := &domain.User{ID: uuid.New(), Role: domain.RoleTenant, Phone: "+919000000502", PropertyID: &propA}
	userStore := &scopeTestUserStore{users: map[uuid.UUID]*domain.User{pending.ID: pending}}

	router := NewRouter(Deps{
		JWTSecret: secret,
		PropertyStore: &scopeTestPropertyStore{props: map[uuid.UUID]*domain.Property{
			propA: {ID: propA, OwnerPhone: "+919000000599"},
		}},
		UserStore:    userStore,
		AuthUserRepo: userStore,
		Waitlist:     stubWaitlistCrossProperty{},
	})

	tok, err := auth.IssueToken(secret, pending)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.NewBufferString(`{"room_id":"` + uuid.New().String() + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/join/waitlist", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}

func TestJoinWaitlist_RoomOpen_Returns409(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "test-secret-key-with-sufficient-length-32"
	propA := uuid.New()
	pending := &domain.User{ID: uuid.New(), Role: domain.RoleTenant, Phone: "+919000000503", PropertyID: &propA}
	userStore := &scopeTestUserStore{users: map[uuid.UUID]*domain.User{pending.ID: pending}}

	router := NewRouter(Deps{
		JWTSecret: secret,
		PropertyStore: &scopeTestPropertyStore{props: map[uuid.UUID]*domain.Property{
			propA: {ID: propA, OwnerPhone: "+919000000599"},
		}},
		UserStore:    userStore,
		AuthUserRepo: userStore,
		Waitlist:     stubWaitlistRoomOpen{},
	})

	tok, err := auth.IssueToken(secret, pending)
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.NewBufferString(`{"room_id":"` + uuid.New().String() + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/join/waitlist", body)
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusConflict {
		t.Fatalf("status %d body %s", w.Code, w.Body.String())
	}
}
