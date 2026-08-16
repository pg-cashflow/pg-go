package join

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
)

type stubProps struct {
	byID     map[uuid.UUID]*domain.Property
	byInvite map[string]*domain.Property
	rotated  string
}

func (s *stubProps) GetByID(_ context.Context, id uuid.UUID) (*domain.Property, error) {
	p, ok := s.byID[id]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	cp := *p
	return &cp, nil
}
func (s *stubProps) GetByInviteCode(_ context.Context, code string) (*domain.Property, error) {
	p, ok := s.byInvite[code]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	cp := *p
	return &cp, nil
}
func (s *stubProps) SetInviteCode(_ context.Context, id uuid.UUID, code string) error {
	s.rotated = code
	if p, ok := s.byID[id]; ok {
		p.InviteCode = code
	}
	return nil
}

type stubJoins struct {
	byID   map[uuid.UUID]*domain.JoinRequest
	byUser map[uuid.UUID]*domain.JoinRequest
	list   []domain.JoinRequest
}

func (s *stubJoins) Create(_ context.Context, j *domain.JoinRequest) error {
	j.ID = uuid.New()
	s.byID[j.ID] = j
	s.byUser[j.UserID] = j
	return nil
}
func (s *stubJoins) GetByID(_ context.Context, id uuid.UUID) (*domain.JoinRequest, error) {
	j, ok := s.byID[id]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return j, nil
}
func (s *stubJoins) GetPendingByUser(_ context.Context, userID uuid.UUID) (*domain.JoinRequest, error) {
	j, ok := s.byUser[userID]
	if !ok || j.Status != domain.JoinPending {
		return nil, pgx.ErrNoRows
	}
	return j, nil
}
func (s *stubJoins) GetLatestByUser(_ context.Context, userID uuid.UUID) (*domain.JoinRequest, error) {
	j, ok := s.byUser[userID]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	return j, nil
}
func (s *stubJoins) ListByProperty(context.Context, uuid.UUID, *domain.JoinStatus) ([]domain.JoinRequest, error) {
	return s.list, nil
}
func (s *stubJoins) Update(_ context.Context, j *domain.JoinRequest) error {
	s.byID[j.ID] = j
	s.byUser[j.UserID] = j
	return nil
}

type stubUsers struct{ linked uuid.UUID }

func (s *stubUsers) SetTenantID(_ context.Context, userID, tenantID uuid.UUID) error {
	s.linked = tenantID
	return nil
}

type stubTenants struct {
	created *domain.Tenant
	lastIn  domain.NewTenantInput
}

func (s *stubTenants) CreateTenant(_ context.Context, in domain.NewTenantInput, depositPaise int) (*domain.Tenant, error) {
	s.lastIn = in
	s.created = &domain.Tenant{
		ID:           uuid.New(),
		PropertyID:   in.PropertyID,
		Name:         in.Name,
		Phone:        in.Phone,
		RoomNumber:   in.RoomNumber,
		AadhaarLast4: in.AadhaarLast4,
		RentAmount:   in.RentAmount,
		DueDay:       in.DueDay,
		Status:       domain.TenantStatusActive,
	}
	_ = depositPaise
	return s.created, nil
}

func TestLookupInvite(t *testing.T) {
	pid := uuid.New()
	props := &stubProps{byInvite: map[string]*domain.Property{
		"ABCD1234": {ID: pid, Name: "Dev PG", OwnerName: "Owner"},
	}}
	svc := NewService(props, &stubJoins{}, &stubUsers{}, &stubTenants{}, events.NoopPublisher{})
	p, err := svc.LookupInvite(context.Background(), "abcd1234")
	if err != nil || p.ID != pid {
		t.Fatalf("lookup: %v %+v", err, p)
	}
	if _, err := svc.LookupInvite(context.Background(), "NOPE"); err != ErrInvalidInvite {
		t.Fatalf("expected invalid invite, got %v", err)
	}
}

func TestEnsurePendingAndActivate(t *testing.T) {
	pid := uuid.New()
	uid := uuid.New()
	joins := &stubJoins{byID: map[uuid.UUID]*domain.JoinRequest{}, byUser: map[uuid.UUID]*domain.JoinRequest{}}
	users := &stubUsers{}
	tenants := &stubTenants{}
	svc := NewService(&stubProps{byID: map[uuid.UUID]*domain.Property{pid: {ID: pid}}}, joins, users, tenants, events.NoopPublisher{})

	user := &domain.User{ID: uid, Phone: "+919999000002", Role: domain.RoleTenant, PropertyID: &pid}
	j, err := svc.EnsurePending(context.Background(), user, pid)
	if err != nil {
		t.Fatal(err)
	}
	j.Name = "Ram"
	if err := joins.Update(context.Background(), j); err != nil {
		t.Fatal(err)
	}

	got, err := svc.Activate(context.Background(), pid, j.ID, ActivateInput{
		RentAmount: 1500000, DueDay: 5, DepositAmount: 1500000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Ram" || users.linked != got.ID {
		t.Fatalf("tenant=%+v linked=%s", got, users.linked)
	}
	if j.Status != domain.JoinApproved {
		t.Fatalf("status=%s", j.Status)
	}
}

func TestActivateRejectsMissingName(t *testing.T) {
	pid := uuid.New()
	j := &domain.JoinRequest{ID: uuid.New(), PropertyID: pid, UserID: uuid.New(), Status: domain.JoinPending}
	joins := &stubJoins{byID: map[uuid.UUID]*domain.JoinRequest{j.ID: j}, byUser: map[uuid.UUID]*domain.JoinRequest{}}
	svc := NewService(&stubProps{}, joins, &stubUsers{}, &stubTenants{}, events.NoopPublisher{})
	if _, err := svc.Activate(context.Background(), pid, j.ID, ActivateInput{RentAmount: 1, DueDay: 1}); err != ErrNameRequired {
		t.Fatalf("got %v", err)
	}
}

func TestActivateCopiesAadhaar(t *testing.T) {
	pid := uuid.New()
	uid := uuid.New()
	last4 := "9012"
	j := &domain.JoinRequest{
		ID: uuid.New(), PropertyID: pid, UserID: uid, Name: "Ram", Phone: "+919999000002",
		Status: domain.JoinPending, AadhaarLast4: &last4,
	}
	joins := &stubJoins{byID: map[uuid.UUID]*domain.JoinRequest{j.ID: j}, byUser: map[uuid.UUID]*domain.JoinRequest{uid: j}}
	tenants := &stubTenants{}
	svc := NewService(&stubProps{}, joins, &stubUsers{}, tenants, events.NoopPublisher{})
	got, err := svc.Activate(context.Background(), pid, j.ID, ActivateInput{RentAmount: 1500000, DueDay: 5})
	if err != nil {
		t.Fatal(err)
	}
	if tenants.lastIn.AadhaarLast4 == nil || *tenants.lastIn.AadhaarLast4 != last4 {
		t.Fatalf("input aadhaar=%v", tenants.lastIn.AadhaarLast4)
	}
	if got.AadhaarLast4 == nil || *got.AadhaarLast4 != last4 {
		t.Fatalf("tenant aadhaar=%v", got.AadhaarLast4)
	}
}

type failLinkUsers struct{ err error }

func (s *failLinkUsers) SetTenantID(context.Context, uuid.UUID, uuid.UUID) error { return s.err }

func TestActivateLinkFailureLeavesTenantOnNonPoolPath(t *testing.T) {
	pid := uuid.New()
	j := &domain.JoinRequest{
		ID: uuid.New(), PropertyID: pid, UserID: uuid.New(), Name: "Ram", Phone: "+919999000002",
		Status: domain.JoinPending,
	}
	joins := &stubJoins{byID: map[uuid.UUID]*domain.JoinRequest{j.ID: j}, byUser: map[uuid.UUID]*domain.JoinRequest{}}
	tenants := &stubTenants{}
	svc := NewService(&stubProps{}, joins, &failLinkUsers{err: errors.New("link boom")}, tenants, events.NoopPublisher{})
	if _, err := svc.Activate(context.Background(), pid, j.ID, ActivateInput{RentAmount: 1, DueDay: 1}); err == nil {
		t.Fatal("expected link error")
	}
	if tenants.created == nil {
		t.Fatal("non-pool path creates tenant before link; leftover is expected without a pool")
	}
	if j.Status != domain.JoinPending {
		t.Fatalf("join status=%s", j.Status)
	}
}
