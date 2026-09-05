package join

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
	"github.com/pg-cashflow/pg-go/internal/tenant"
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
	due := in.DueDay
	s.created = &domain.Tenant{
		ID:           uuid.New(),
		PropertyID:   in.PropertyID,
		Name:         in.Name,
		Phone:        in.Phone,
		RoomNumber:   in.RoomNumber,
		AadhaarLast4: in.AadhaarLast4,
		RentAmount:   in.RentAmount,
		DueDay:       &due,
		Status:       domain.TenantStatusActive,
	}
	_ = depositPaise
	return s.created, nil
}

type memTenantRepo struct {
	byID map[uuid.UUID]*domain.Tenant
}

func (m *memTenantRepo) Create(_ context.Context, t *domain.Tenant) error {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	cp := *t
	if len(t.IDPhotoBytes) > 0 {
		cp.HasIDPhoto = true
		cp.IDPhotoBytes = append([]byte(nil), t.IDPhotoBytes...)
	}
	if m.byID == nil {
		m.byID = map[uuid.UUID]*domain.Tenant{}
	}
	m.byID[t.ID] = &cp
	return nil
}
func (m *memTenantRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
	t, ok := m.byID[id]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	cp := *t
	return &cp, nil
}
func (m *memTenantRepo) Update(_ context.Context, t *domain.Tenant) error {
	cp := *t
	m.byID[t.ID] = &cp
	return nil
}

type depositBilling struct {
	created int
}

func (d *depositBilling) CreateDepositDue(_ context.Context, tenant *domain.Tenant, amountPaise int) (*domain.Due, error) {
	d.created++
	return &domain.Due{ID: uuid.New(), TenantID: tenant.ID, Amount: amountPaise, Kind: domain.DueKindDeposit}, nil
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

func TestCompleteOnboardingAndActivate(t *testing.T) {
	pid := uuid.New()
	uid := uuid.New()
	joins := &stubJoins{byID: map[uuid.UUID]*domain.JoinRequest{}, byUser: map[uuid.UUID]*domain.JoinRequest{}}
	users := &stubUsers{}
	repo := &memTenantRepo{}
	bill := &depositBilling{}
	tenantSvc := tenant.NewService(repo, nil, bill, events.NoopPublisher{})
	svc := NewService(&stubProps{byID: map[uuid.UUID]*domain.Property{pid: {ID: pid}}}, joins, users, &stubTenants{}, events.NoopPublisher{})
	svc.tenantSvc = tenantSvc

	user := &domain.User{ID: uid, Phone: "+919999000002", Role: domain.RoleTenant, PropertyID: &pid}
	j, err := svc.EnsurePending(context.Background(), user, pid)
	if err != nil {
		t.Fatal(err)
	}

	jOut, ten, err := svc.CompleteOnboarding(context.Background(), uid, ProfileInput{
		Name: "Ram", PermanentAddress: "Home St", CurrentAddress: "PG Block A",
		ParentName: "Suresh", EmergencyPhone: "+919888777666", Consent: true,
		IDPhotoBytes: []byte{0xff, 0xd8, 0xff},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ten.Status != domain.TenantStatusPendingAllocation || ten.RentAmount != 0 || ten.DueDay != nil {
		t.Fatalf("pending tenant=%+v", ten)
	}
	if !ten.HasIDPhoto {
		t.Fatal("expected has_id_photo")
	}
	if users.linked != ten.ID {
		t.Fatalf("linked=%s want %s", users.linked, ten.ID)
	}
	if jOut.Status != domain.JoinApproved || jOut.TenantID == nil {
		t.Fatalf("join=%+v", jOut)
	}
	if bill.created != 0 {
		t.Fatal("no deposit due until assign terms")
	}

	got, err := svc.Activate(context.Background(), pid, j.ID, ActivateInput{
		RentAmount: 1500000, DueDay: 5, DepositAmount: 1500000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TenantStatusActive || got.RentAmount != 1500000 || got.DueDay == nil || *got.DueDay != 5 {
		t.Fatalf("active tenant=%+v", got)
	}
	if bill.created != 1 {
		t.Fatalf("deposit dues=%d", bill.created)
	}
}

func TestCompleteOnboardingRequiresPhotoAndFields(t *testing.T) {
	pid := uuid.New()
	uid := uuid.New()
	joins := &stubJoins{byID: map[uuid.UUID]*domain.JoinRequest{}, byUser: map[uuid.UUID]*domain.JoinRequest{}}
	repo := &memTenantRepo{}
	tenantSvc := tenant.NewService(repo, nil, &depositBilling{}, events.NoopPublisher{})
	svc := NewService(&stubProps{}, joins, &stubUsers{}, &stubTenants{}, events.NoopPublisher{})
	svc.tenantSvc = tenantSvc

	user := &domain.User{ID: uid, Phone: "+919999000002", Role: domain.RoleTenant, PropertyID: &pid}
	if _, err := svc.EnsurePending(context.Background(), user, pid); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.CompleteOnboarding(context.Background(), uid, ProfileInput{
		Name: "Ram", Consent: true, IDPhotoBytes: []byte{1},
	}); err != ErrProfileIncomplete {
		t.Fatalf("got %v", err)
	}
	if _, _, err := svc.CompleteOnboarding(context.Background(), uid, ProfileInput{
		Name: "Ram", PermanentAddress: "a", CurrentAddress: "b", ParentName: "c", EmergencyPhone: "d",
		Consent: true,
	}); err != ErrPhotoRequired {
		t.Fatalf("got %v", err)
	}
}

func TestActivateRejectsPendingJoin(t *testing.T) {
	pid := uuid.New()
	j := &domain.JoinRequest{ID: uuid.New(), PropertyID: pid, UserID: uuid.New(), Status: domain.JoinPending, Name: "Ram"}
	joins := &stubJoins{byID: map[uuid.UUID]*domain.JoinRequest{j.ID: j}, byUser: map[uuid.UUID]*domain.JoinRequest{}}
	svc := NewService(&stubProps{}, joins, &stubUsers{}, &stubTenants{}, events.NoopPublisher{})
	svc.tenantSvc = tenant.NewService(&memTenantRepo{}, nil, &depositBilling{}, events.NoopPublisher{})
	if _, err := svc.Activate(context.Background(), pid, j.ID, ActivateInput{RentAmount: 1, DueDay: 1}); err != ErrNotAwaitingAssign {
		t.Fatalf("got %v", err)
	}
}

func TestRejectOnlyPending(t *testing.T) {
	pid := uuid.New()
	j := &domain.JoinRequest{ID: uuid.New(), PropertyID: pid, UserID: uuid.New(), Status: domain.JoinPending}
	joins := &stubJoins{byID: map[uuid.UUID]*domain.JoinRequest{j.ID: j}, byUser: map[uuid.UUID]*domain.JoinRequest{}}
	svc := NewService(&stubProps{}, joins, &stubUsers{}, &stubTenants{}, events.NoopPublisher{})
	if err := svc.Reject(context.Background(), pid, j.ID); err != nil {
		t.Fatal(err)
	}
	if j.Status != domain.JoinRejected {
		t.Fatalf("status=%s", j.Status)
	}
}

func TestActivateLinkNotNeeded(t *testing.T) {
	// Assign-terms path does not re-link users; leftover CreateTenant path is gone.
	pid := uuid.New()
	tid := uuid.New()
	j := &domain.JoinRequest{
		ID: uuid.New(), PropertyID: pid, UserID: uuid.New(), Name: "Ram", Phone: "+919999000002",
		Status: domain.JoinApproved, TenantID: &tid,
	}
	joins := &stubJoins{byID: map[uuid.UUID]*domain.JoinRequest{j.ID: j}, byUser: map[uuid.UUID]*domain.JoinRequest{}}
	repo := &memTenantRepo{byID: map[uuid.UUID]*domain.Tenant{
		tid: {ID: tid, PropertyID: pid, Name: "Ram", Status: domain.TenantStatusPendingAllocation, RentAmount: 0},
	}}
	bill := &depositBilling{}
	svc := NewService(&stubProps{}, joins, &failLinkUsers{err: errors.New("should not link")}, &stubTenants{}, events.NoopPublisher{})
	svc.tenantSvc = tenant.NewService(repo, nil, bill, events.NoopPublisher{})
	got, err := svc.Activate(context.Background(), pid, j.ID, ActivateInput{RentAmount: 100, DueDay: 5})
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != domain.TenantStatusActive {
		t.Fatalf("status=%s", got.Status)
	}
}

type failLinkUsers struct{ err error }

func (s *failLinkUsers) SetTenantID(context.Context, uuid.UUID, uuid.UUID) error { return s.err }
