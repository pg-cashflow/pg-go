package tenant

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
)

type memTenants struct {
	byID map[uuid.UUID]*domain.Tenant
}

func (m *memTenants) Create(_ context.Context, t *domain.Tenant) error {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	cp := *t
	if m.byID == nil {
		m.byID = map[uuid.UUID]*domain.Tenant{}
	}
	m.byID[t.ID] = &cp
	return nil
}

func (m *memTenants) GetByID(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
	t, ok := m.byID[id]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	cp := *t
	return &cp, nil
}

func (m *memTenants) Update(_ context.Context, t *domain.Tenant) error {
	cp := *t
	m.byID[t.ID] = &cp
	return nil
}

type memPush struct {
	deleted []uuid.UUID
}

func (m *memPush) DeleteByTenant(_ context.Context, tenantID uuid.UUID) error {
	m.deleted = append(m.deleted, tenantID)
	return nil
}

type noopBilling struct{}

func (noopBilling) CreateDepositDue(context.Context, *domain.Tenant, int) (*domain.Due, error) {
	return &domain.Due{ID: uuid.New(), Kind: domain.DueKindDeposit}, nil
}

type recPub struct {
	evts []domain.Event
}

func (p *recPub) Publish(_ context.Context, e domain.Event) error {
	p.evts = append(p.evts, e)
	return nil
}

var _ events.Publisher = (*recPub)(nil)

func TestUpdateTenantRentChangeDoesNotTouchDues(t *testing.T) {
	id := uuid.New()
	prop := uuid.New()
	repo := &memTenants{byID: map[uuid.UUID]*domain.Tenant{
		id: {ID: id, PropertyID: prop, Name: "A", RentAmount: 10000, DueDay: 5, Status: domain.TenantStatusActive},
	}}
	pub := &recPub{}
	// Snapshot of an "existing due" that must remain untouched by UpdateTenant.
	existingDueAmount := 10000
	svc := NewService(repo, &memPush{}, noopBilling{}, pub)

	updated := &domain.Tenant{
		ID: id, PropertyID: prop, Name: "A", RentAmount: 12000, DueDay: 5, Status: domain.TenantStatusActive,
	}
	if err := svc.UpdateTenant(context.Background(), updated); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.GetByID(context.Background(), id)
	if got.RentAmount != 12000 {
		t.Fatalf("rent not updated: %d", got.RentAmount)
	}
	// Due amount is not stored in tenant service — assert we only published the event
	// and never received a due-mutating collaborator (noop billing, no due repo).
	if existingDueAmount != 10000 {
		t.Fatal("due amount mutated unexpectedly")
	}
	var saw bool
	for _, e := range pub.evts {
		if e.EventType == domain.EvtRentAmountChanged {
			saw = true
			var p domain.RentAmountChangedPayload
			_ = json.Unmarshal(e.Payload, &p)
			if p.OldPaise != 10000 || p.NewPaise != 12000 {
				t.Fatalf("payload=%+v", p)
			}
		}
	}
	if !saw {
		t.Fatal("missing RentAmountChanged")
	}
}

func TestUpdateTenantNoEventWhenRentUnchanged(t *testing.T) {
	id := uuid.New()
	prop := uuid.New()
	repo := &memTenants{byID: map[uuid.UUID]*domain.Tenant{
		id: {ID: id, PropertyID: prop, Name: "A", RentAmount: 10000, DueDay: 5},
	}}
	pub := &recPub{}
	svc := NewService(repo, nil, nil, pub)
	if err := svc.UpdateTenant(context.Background(), &domain.Tenant{
		ID: id, PropertyID: prop, Name: "B", RentAmount: 10000, DueDay: 5,
	}); err != nil {
		t.Fatal(err)
	}
	for _, e := range pub.evts {
		if e.EventType == domain.EvtRentAmountChanged {
			t.Fatal("should not publish RentAmountChanged")
		}
	}
}
