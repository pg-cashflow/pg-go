package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/sms"
)

func TestIssueTokenForVerifiedPhone_TenantStatus(t *testing.T) {
	phone := "+919876543210"
	tests := []struct {
		name    string
		status  domain.TenantStatus
		wantErr error
	}{
		{"active", domain.TenantStatusActive, nil},
		{"pending_allocation", domain.TenantStatusPendingAllocation, nil},
		{"vacated", domain.TenantStatusVacated, ErrTenantVacated},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tenantID := uuid.New()
			propID := uuid.New()
			userID := uuid.New()
			users := &stubUsers{byPhone: map[string]*domain.User{
				phone: {
					ID: userID, Phone: phone, Role: domain.RoleTenant,
					TenantID: &tenantID, PropertyID: &propID, TokenVersion: 1,
				},
			}}
			tenants := &stubTenantsAuth{byID: map[uuid.UUID]*domain.Tenant{
				tenantID: {ID: tenantID, PropertyID: propID, Status: tt.status},
			}}
			svc := NewService(nil, users, tenants, stubProps{}, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")

			_, _, err := svc.IssueTokenForVerifiedPhone(context.Background(), phone)
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("want %v, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}
