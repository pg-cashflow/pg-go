package tenant

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func strPtr(s string) *string {
	return &s
}

func TestPhoneNormalization_CreateTenant(t *testing.T) {
	tests := []struct {
		name      string
		rawPhone  *string
		wantPhone *string
		wantErr   bool
	}{
		{
			name:      "nil phone is permitted (phoneless tenant)",
			rawPhone:  nil,
			wantPhone: nil,
			wantErr:   false,
		},
		{
			name:      "empty phone is permitted",
			rawPhone:  strPtr(""),
			wantPhone: nil,
			wantErr:   false,
		},
		{
			name:      "whitespace phone is permitted",
			rawPhone:  strPtr("   "),
			wantPhone: nil,
			wantErr:   false,
		},
		{
			name:      "10-digit Indian mobile normalizes to E.164",
			rawPhone:  strPtr("9876543210"),
			wantPhone: strPtr("+919876543210"),
			wantErr:   false,
		},
		{
			name:      "11-digit with leading zero normalizes to E.164",
			rawPhone:  strPtr("09876543210"),
			wantPhone: strPtr("+919876543210"),
			wantErr:   false,
		},
		{
			name:      "12-digit with 91 prefix normalizes to E.164",
			rawPhone:  strPtr("919876543210"),
			wantPhone: strPtr("+919876543210"),
			wantErr:   false,
		},
		{
			name:      "strict international E.164 preserved",
			rawPhone:  strPtr("+14155552671"),
			wantPhone: strPtr("+14155552671"),
			wantErr:   false,
		},
		{
			name:     "invalid short phone rejected",
			rawPhone: strPtr("12345"),
			wantErr:  true,
		},
		{
			name:     "invalid non-digit phone rejected",
			rawPhone: strPtr("notaphone"),
			wantErr:  true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			repo := &memTenants{byID: map[uuid.UUID]*domain.Tenant{}}
			svc := NewService(repo, nil, noopBilling{}, &recPub{})
			in := domain.NewTenantInput{
				PropertyID: uuid.New(),
				Name:       "Test Tenant",
				Phone:      tc.rawPhone,
				RentAmount: 1000000,
				DueDay:     5,
			}
			created, err := svc.CreateTenant(context.Background(), in, 1000000)
			if tc.wantErr {
				if err != ErrPhoneFormat {
					t.Fatalf("expected ErrPhoneFormat, got %v", err)
				}
				if created != nil {
					t.Fatalf("expected nil created tenant on error, got %+v", created)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantPhone == nil {
				if created.Phone != nil {
					t.Fatalf("expected nil phone, got %v", *created.Phone)
				}
			} else {
				if created.Phone == nil || *created.Phone != *tc.wantPhone {
					t.Fatalf("expected phone %v, got %v", *tc.wantPhone, created.Phone)
				}
			}
		})
	}
}

func TestPhoneNormalization_AttachPhone(t *testing.T) {
	id := uuid.New()
	repo := &memTenants{byID: map[uuid.UUID]*domain.Tenant{
		id: {
			ID:         id,
			PropertyID: uuid.New(),
			Name:       "Test Tenant",
			RentAmount: 1000000,
			DueDay:     domain.Int16Ptr(5),
		},
	}}
	svc := NewService(repo, nil, noopBilling{}, &recPub{})

	// Rejection should not mutate
	if err := svc.AttachPhone(context.Background(), id, "bad123"); err != ErrPhoneFormat {
		t.Fatalf("expected ErrPhoneFormat, got %v", err)
	}
	tenantAfterBad, _ := repo.GetByID(context.Background(), id)
	if tenantAfterBad.Phone != nil {
		t.Fatalf("expected phone to remain nil after failed attach, got %v", *tenantAfterBad.Phone)
	}

	// Successful attachment with 10-digit number
	if err := svc.AttachPhone(context.Background(), id, "9876543210"); err != nil {
		t.Fatalf("unexpected attach error: %v", err)
	}
	tenantAfterGood, _ := repo.GetByID(context.Background(), id)
	if tenantAfterGood.Phone == nil || *tenantAfterGood.Phone != "+919876543210" {
		t.Fatalf("expected +919876543210, got %v", tenantAfterGood.Phone)
	}
}
