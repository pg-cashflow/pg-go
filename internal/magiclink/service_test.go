package magiclink

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockTokenRepo struct {
	tokens map[string]*domain.PaymentToken
}

func (m *mockTokenRepo) Create(_ context.Context, t *domain.PaymentToken) error {
	if m.tokens == nil {
		m.tokens = make(map[string]*domain.PaymentToken)
	}
	m.tokens[t.TokenHash] = t
	return nil
}

func (m *mockTokenRepo) InvalidateUnusedForDue(_ context.Context, dueID uuid.UUID) error {
	for _, t := range m.tokens {
		if t.DueID == dueID && !t.Used {
			t.Used = true
		}
	}
	return nil
}

func (m *mockTokenRepo) GetByHash(_ context.Context, hash string) (*domain.PaymentToken, error) {
	t, ok := m.tokens[hash]
	if !ok {
		return nil, ErrTokenNotFound
	}
	return t, nil
}

func (m *mockTokenRepo) MarkUsed(_ context.Context, id uuid.UUID) error {
	for _, t := range m.tokens {
		if t.ID == id {
			if t.Used {
				return domain.ErrTokenAlreadyUsed
			}
			t.Used = true
			return nil
		}
	}
	return ErrTokenNotFound
}

type mockDueRepo struct {
	dues map[uuid.UUID]*domain.Due
}

func (m *mockDueRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Due, error) {
	d, ok := m.dues[id]
	if !ok {
		return nil, errors.New("due not found")
	}
	return d, nil
}

type mockPropertyRepo struct {
	props map[uuid.UUID]*domain.Property
}

func (m *mockPropertyRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Property, error) {
	p, ok := m.props[id]
	if !ok {
		return nil, errors.New("property not found")
	}
	return p, nil
}

type mockTenantRepo struct {
	tenants map[uuid.UUID]*domain.Tenant
}

func (m *mockTenantRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
	t, ok := m.tenants[id]
	if !ok {
		return nil, errors.New("tenant not found")
	}
	return t, nil
}

func TestResolveToken_PaidDueReturnsErrTokenUsed(t *testing.T) {
	secret := "secret-key-32-chars-test-long!"
	dueID := uuid.New()
	propID := uuid.New()
	tenantID := uuid.New()

	raw := "mockrawtoken1234567890abcdef"
	hash := HashToken(secret, raw)

	tokens := &mockTokenRepo{
		tokens: map[string]*domain.PaymentToken{
			hash: {
				ID:        uuid.New(),
				DueID:     dueID,
				TokenHash: hash,
				ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
				Used:      false,
			},
		},
	}

	dueRepo := &mockDueRepo{
		dues: map[uuid.UUID]*domain.Due{
			dueID: {
				ID:         dueID,
				PropertyID: propID,
				TenantID:   tenantID,
				Amount:     0,
				Status:     domain.DueStatusPaid,
			},
		},
	}

	propRepo := &mockPropertyRepo{
		props: map[uuid.UUID]*domain.Property{
			propID: {ID: propID, Name: "Test PG", OwnerName: "Owner A"},
		},
	}

	tenantRepo := &mockTenantRepo{
		tenants: map[uuid.UUID]*domain.Tenant{
			tenantID: {ID: tenantID, Name: "Tenant T"},
		},
	}

	svc := NewService(tokens, dueRepo, propRepo, tenantRepo, secret)

	_, err := svc.ResolveToken(context.Background(), raw)
	if err == nil {
		t.Fatal("expected error for paid due, got nil")
	}
	if !errors.Is(err, ErrTokenUsed) {
		t.Fatalf("expected ErrTokenUsed, got %v", err)
	}
}

func TestResolveToken_UnpaidDueSucceeds(t *testing.T) {
	secret := "secret-key-32-chars-test-long!"
	dueID := uuid.New()
	propID := uuid.New()
	tenantID := uuid.New()

	raw := "mockrawtokenunpaid1234567890"
	hash := HashToken(secret, raw)

	tokens := &mockTokenRepo{
		tokens: map[string]*domain.PaymentToken{
			hash: {
				ID:        uuid.New(),
				DueID:     dueID,
				TokenHash: hash,
				ExpiresAt: time.Now().UTC().Add(24 * time.Hour),
				Used:      false,
			},
		},
	}

	dueRepo := &mockDueRepo{
		dues: map[uuid.UUID]*domain.Due{
			dueID: {
				ID:         dueID,
				PropertyID: propID,
				TenantID:   tenantID,
				Amount:     500000,
				Status:     domain.DueStatusPending,
			},
		},
	}

	propRepo := &mockPropertyRepo{
		props: map[uuid.UUID]*domain.Property{
			propID: {ID: propID, Name: "Test PG", OwnerName: "Owner A"},
		},
	}

	tenantRepo := &mockTenantRepo{
		tenants: map[uuid.UUID]*domain.Tenant{
			tenantID: {ID: tenantID, Name: "Tenant T"},
		},
	}

	svc := NewService(tokens, dueRepo, propRepo, tenantRepo, secret)

	view, err := svc.ResolveToken(context.Background(), raw)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if view.AmountPaise != 500000 {
		t.Fatalf("expected 500000, got %d", view.AmountPaise)
	}
	if view.TenantName != "Tenant T" {
		t.Fatalf("expected Tenant T, got %s", view.TenantName)
	}
}
