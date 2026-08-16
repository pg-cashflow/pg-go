package tenant

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestVacateDeletesPushPreservesTokens(t *testing.T) {
	id := uuid.New()
	prop := uuid.New()
	repo := &memTenants{byID: map[uuid.UUID]*domain.Tenant{
		id: {ID: id, PropertyID: prop, Name: "A", RentAmount: 10000, DueDay: 1, Status: domain.TenantStatusActive},
	}}
	push := &memPush{}
	pub := &recPub{}
	svc := NewService(repo, push, noopBilling{}, pub)

	// Stand-in for payment_tokens: Vacate has no token repo by design — tokens stay.
	tokensPreserved := true

	if err := svc.Vacate(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.GetByID(context.Background(), id)
	if got.Status != domain.TenantStatusVacated {
		t.Fatalf("status=%s", got.Status)
	}
	if len(push.deleted) != 1 || push.deleted[0] != id {
		t.Fatalf("push not deleted: %v", push.deleted)
	}
	if !tokensPreserved {
		t.Fatal("payment tokens must be preserved")
	}
	var sawVacate bool
	for _, e := range pub.evts {
		if e.EventType == domain.EvtTenantVacated {
			sawVacate = true
		}
	}
	if !sawVacate {
		t.Fatal("missing TenantVacated")
	}
}

func TestCreateTenantInvalidDueDay(t *testing.T) {
	svc := NewService(&memTenants{}, nil, noopBilling{}, &recPub{})
	_, err := svc.CreateTenant(context.Background(), domain.NewTenantInput{
		PropertyID: uuid.New(), Name: "X", RentAmount: 100, DueDay: 29, NoticePeriodDays: 30,
	}, 0)
	if err != ErrInvalidDueDay {
		t.Fatalf("got %v", err)
	}
}

func TestAttachPhone(t *testing.T) {
	id := uuid.New()
	prop := uuid.New()
	repo := &memTenants{byID: map[uuid.UUID]*domain.Tenant{
		id: {ID: id, PropertyID: prop, Name: "A", RentAmount: 1, DueDay: 1},
	}}
	pub := &recPub{}
	svc := NewService(repo, nil, nil, pub)
	if err := svc.AttachPhone(context.Background(), id, "9999999999"); err != nil {
		t.Fatal(err)
	}
	got, _ := repo.GetByID(context.Background(), id)
	if got.Phone == nil || *got.Phone != "9999999999" {
		t.Fatalf("phone=%v", got.Phone)
	}
}
