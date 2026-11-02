package domain_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestTenant_CanSignIn(t *testing.T) {
	tests := []struct {
		name     string
		tenant   *domain.Tenant
		expected bool
	}{
		{
			name:     "nil tenant cannot sign in",
			tenant:   nil,
			expected: false,
		},
		{
			name: "active tenant can sign in",
			tenant: &domain.Tenant{
				ID:     uuid.New(),
				Status: domain.TenantStatusActive,
			},
			expected: true,
		},
		{
			name: "pending allocation tenant can sign in",
			tenant: &domain.Tenant{
				ID:     uuid.New(),
				Status: domain.TenantStatusPendingAllocation,
			},
			expected: true,
		},
		{
			name: "vacated tenant cannot sign in",
			tenant: &domain.Tenant{
				ID:     uuid.New(),
				Status: domain.TenantStatusVacated,
			},
			expected: false,
		},
		{
			name: "unknown or empty status cannot sign in",
			tenant: &domain.Tenant{
				ID:     uuid.New(),
				Status: domain.TenantStatus("suspended"),
			},
			expected: false,
		},
		{
			name: "empty status cannot sign in",
			tenant: &domain.Tenant{
				ID:     uuid.New(),
				Status: domain.TenantStatus(""),
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.tenant.CanSignIn()
			if got != tt.expected {
				t.Errorf("CanSignIn() = %v; want %v", got, tt.expected)
			}
		})
	}
}

func TestTenant_Payable(t *testing.T) {
	activeTenant := &domain.Tenant{Status: domain.TenantStatusActive}
	pendingTenant := &domain.Tenant{Status: domain.TenantStatusPendingAllocation}
	vacatedTenant := &domain.Tenant{Status: domain.TenantStatusVacated}

	// IsPayable only true for active
	if !activeTenant.IsPayable() {
		t.Errorf("expected active tenant to be payable")
	}
	if pendingTenant.IsPayable() {
		t.Errorf("expected pending allocation tenant not to be payable")
	}
	if vacatedTenant.IsPayable() {
		t.Errorf("expected vacated tenant not to be payable")
	}

	// Payable with KYC flags
	if !activeTenant.Payable(false, false) {
		t.Errorf("active tenant without KYC requirement should be payable")
	}
	if activeTenant.Payable(true, false) {
		t.Errorf("active tenant requiring KYC without verified KYC should not be payable")
	}
	if !activeTenant.Payable(true, true) {
		t.Errorf("active tenant requiring KYC with verified KYC should be payable")
	}
	if pendingTenant.Payable(false, false) {
		t.Errorf("pending tenant should never be payable")
	}
}

func TestTenant_GamificationAndContact(t *testing.T) {
	now := time.Now()
	adultDOB := now.AddDate(-25, 0, 0)
	phone := "+919876543210"

	t1 := &domain.Tenant{
		MajorityDate:           &adultDOB,
		IsGamificationDisabled: false,
		Phone:                  &phone,
	}
	if !t1.GamificationActive(now) {
		t.Errorf("adult tenant without gamification disabled should have gamification active")
	}
	if t1.IsPhoneLess() {
		t.Errorf("tenant with phone should not be phoneless")
	}
	if t1.ContactMode() != "self_service" {
		t.Errorf("tenant with phone should have self_service contact mode")
	}

	t2 := &domain.Tenant{Phone: nil}
	if !t2.IsPhoneLess() {
		t.Errorf("tenant with nil phone should be phoneless")
	}
	if t2.ContactMode() != "cash_only" {
		t.Errorf("phoneless tenant should have cash_only contact mode")
	}
}
