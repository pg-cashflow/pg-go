package notification

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestBackoffDuration(t *testing.T) {
	tests := []struct {
		attempts int
		want     time.Duration
	}{
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 32 * time.Second},
	}

	for _, tt := range tests {
		got := BackoffDuration(tt.attempts)
		if got != tt.want {
			t.Errorf("BackoffDuration(%d) = %v; want %v", tt.attempts, got, tt.want)
		}
	}
}

func TestCursor_RoundTripAndValidation(t *testing.T) {
	validID := uuid.New()
	validTime := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

	cursor := &domain.NotificationCursor{
		CreatedAt: validTime,
		ID:        validID,
	}

	encoded := encodeCursor(cursor)
	decoded, err := decodeCursor(encoded)
	if err != nil {
		t.Fatalf("failed to decode valid cursor: %v", err)
	}
	if decoded.ID != validID || !decoded.CreatedAt.Equal(validTime) {
		t.Fatalf("decoded cursor mismatch: got %+v, want %+v", decoded, cursor)
	}

	// Invalid base64
	_, err = decodeCursor("not-base64!@#$")
	if err == nil {
		t.Fatal("expected error on invalid base64")
	}

	// Malformed JSON (zero time)
	badTime := encodeCursor(&domain.NotificationCursor{ID: validID})
	_, err = decodeCursor(badTime)
	if err == nil {
		t.Fatal("expected error on cursor with zero time")
	}

	// Malformed JSON (nil UUID)
	badID := encodeCursor(&domain.NotificationCursor{CreatedAt: validTime})
	_, err = decodeCursor(badID)
	if err == nil {
		t.Fatal("expected error on cursor with nil UUID")
	}

	// Valid base64 but not JSON
	notJSON := base64.RawURLEncoding.EncodeToString([]byte("plain text"))
	_, err = decodeCursor(notJSON)
	if err == nil {
		t.Fatal("expected error on non-JSON base64")
	}
}

type mockUserLookup struct {
	owners   []domain.User
	managers []domain.User
	tenants  map[uuid.UUID]*domain.User
}

func (m *mockUserLookup) GetByPropertyAndRole(ctx context.Context, propID uuid.UUID, role domain.Role) ([]domain.User, error) {
	if role == domain.RoleOwner {
		return m.owners, nil
	}
	if role == domain.RoleManager {
		return m.managers, nil
	}
	return nil, nil
}

func (m *mockUserLookup) GetByTenantID(ctx context.Context, tenantID uuid.UUID) (*domain.User, error) {
	if u, ok := m.tenants[tenantID]; ok {
		return u, nil
	}
	return nil, nil
}

func (m *mockUserLookup) GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	return nil, nil
}

func TestResolver_EventMappings(t *testing.T) {
	ownerID := uuid.New()
	managerID := uuid.New()
	tenantUserID := uuid.New()
	tenantID := uuid.New()
	propID := uuid.New()

	lookup := &mockUserLookup{
		owners:   []domain.User{{ID: ownerID, Role: domain.RoleOwner}},
		managers: []domain.User{{ID: managerID, Role: domain.RoleManager}},
		tenants: map[uuid.UUID]*domain.User{
			tenantID: {ID: tenantUserID, Role: domain.RoleTenant},
		},
	}

	resolver := NewResolver(lookup)
	ctx := context.Background()

	// 1. Owner-directed: payment report submitted
	notifs, err := resolver.Resolve(ctx, &domain.OutboxEvent{
		EventType:  string(domain.EvtPaymentReportSubmitted),
		PropertyID: propID,
	})
	if err != nil {
		t.Fatalf("resolve payment report: %v", err)
	}
	if len(notifs) != 1 || notifs[0].RecipientID != ownerID || !notifs[0].IsActionRequired {
		t.Fatalf("unexpected owner notifs: %+v", notifs)
	}

	// 2. Owner + Manager directed: hazard reported with deep link
	hazardEvt := &domain.OutboxEvent{
		EventType:  string(domain.EvtHazardReported),
		PropertyID: propID,
		Payload:    []byte(`{"hazard_id":"haz-123"}`),
	}
	hazardNotifs, err := resolver.Resolve(ctx, hazardEvt)
	if err != nil {
		t.Fatalf("resolve hazard: %v", err)
	}
	if len(hazardNotifs) != 2 {
		t.Fatalf("expected 2 recipients (owner + manager), got %d", len(hazardNotifs))
	}
	if hazardNotifs[0].DeepLink != "/owner/hazards/haz-123" {
		t.Fatalf("expected deep link with ID, got %q", hazardNotifs[0].DeepLink)
	}

	// 3. Unknown event skips silently
	unknownNotifs, err := resolver.Resolve(ctx, &domain.OutboxEvent{
		EventType: "some.random.event",
	})
	if err != nil || len(unknownNotifs) != 0 {
		t.Fatalf("expected nil, nil on unknown event, got notifs=%v, err=%v", unknownNotifs, err)
	}
}
