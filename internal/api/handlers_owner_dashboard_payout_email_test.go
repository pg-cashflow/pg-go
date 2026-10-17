package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// payoutEmailProbeUserStore is a minimal UserStore fake scoped to this test
// file (deliberately not reusing fakeOwnerUserStore from
// handlers_owner_sensitive_test.go to avoid coupling an unrelated test's
// fixture to this one).
type payoutEmailProbeUserStore struct {
	users map[uuid.UUID]*domain.User
}

func (s *payoutEmailProbeUserStore) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	u, ok := s.users[id]
	if !ok {
		return nil, fmt.Errorf("user not found")
	}
	cp := *u
	return &cp, nil
}
func (s *payoutEmailProbeUserStore) GetByPhone(_ context.Context, _ string) (*domain.User, error) {
	return nil, fmt.Errorf("not implemented")
}
func (s *payoutEmailProbeUserStore) GetByPropertyAndRole(_ context.Context, _ uuid.UUID, _ domain.Role) ([]domain.User, error) {
	return nil, nil
}
func (s *payoutEmailProbeUserStore) Create(_ context.Context, _ *domain.User) error { return nil }
func (s *payoutEmailProbeUserStore) SetPropertyID(_ context.Context, _, _ uuid.UUID) error {
	return nil
}

// TestOwnerDashboardSummary_PayoutEmailMissing locks in the standing
// payout-delivery-channel nag: an owner with no email on file must see
// payout_email_missing=true on every dashboard load, not just via the
// one-time pre-launch SQL audit in the auth contract doc. An owner with an
// email must see false. This is the gap closed alongside the auth module
// review — without it, an owner who signs up post-launch with no email
// only discovers the problem when a payout approval OTP request 422s.
func TestOwnerDashboardSummary_PayoutEmailMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	jwtSecret := "test-secret-key-32-bytes-long!!"

	run := func(t *testing.T, ownerEmail string, wantMissing bool) {
		propID := uuid.New()
		ownerID := uuid.New()
		ownerUser := &domain.User{ID: ownerID, PropertyID: &propID, Role: domain.RoleOwner, Email: ownerEmail}
		token, err := auth.IssueToken(jwtSecret, ownerUser)
		if err != nil {
			t.Fatalf("issue token: %v", err)
		}

		d := Deps{
			PropertyStore: &dashboardPropStore{props: map[uuid.UUID]*domain.Property{propID: {ID: propID, Name: "Test PG"}}},
			TenantStore:   &dashboardTenantStore{tenants: map[uuid.UUID]*domain.Tenant{}},
			UserStore:     &payoutEmailProbeUserStore{users: map[uuid.UUID]*domain.User{ownerID: ownerUser}},
			JWTSecret:     jwtSecret,
		}
		r := NewRouter(d)

		req := httptest.NewRequest(http.MethodGet, "/api/owner/dashboard/summary", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("expected 200 OK, got %d: %s", w.Code, w.Body.String())
		}
		var res OwnerDashboardSummaryResponse
		if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
			t.Fatalf("unmarshal response failed: %v", err)
		}
		if res.PayoutEmailMissing != wantMissing {
			t.Errorf("payout_email_missing = %v, want %v (owner email=%q)", res.PayoutEmailMissing, wantMissing, ownerEmail)
		}
	}

	t.Run("owner with no email on file is flagged", func(t *testing.T) {
		run(t, "", true)
	})
	t.Run("owner with an email on file is not flagged", func(t *testing.T) {
		run(t, "owner@example.com", false)
	})
}
