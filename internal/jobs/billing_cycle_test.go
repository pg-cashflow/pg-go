package jobs

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var errOpenDue = errors.New("open due exists")

type stubBilling struct {
	calls int
	err   error
	due   *domain.Due
}

func (s *stubBilling) CreateRentDue(ctx context.Context, tenant *domain.Tenant) (*domain.Due, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.due, nil
}

type stubMagicLink struct {
	calls int
}

func (s *stubMagicLink) CreatePaymentToken(ctx context.Context, dueID uuid.UUID) (string, error) {
	s.calls++
	return "/p/token", nil
}

type stubSMS struct {
	n int
}

func (s *stubSMS) Send(ctx context.Context, phone, message string) error {
	s.n++
	return nil
}

func TestBillingCycle_SkipVacated(t *testing.T) {
	billing := &stubBilling{due: &domain.Due{ID: uuid.New(), Amount: 10000}}
	magic := &stubMagicLink{}
	sms := &stubSMS{}
	job := &BillingCycle{
		Billing:          billing,
		MagicLink:        magic,
		SMS:              sms,
		ErrOpenDueExists: errOpenDue,
	}
	phone := "9876543210"
	err := job.processTenant(context.Background(), domain.Tenant{
		ID:     uuid.New(),
		Phone:  &phone,
		Status: domain.TenantStatusVacated,
	})
	if err != nil {
		t.Fatalf("processTenant: %v", err)
	}
	if billing.calls != 0 {
		t.Fatalf("expected vacated skip before CreateRentDue, got %d calls", billing.calls)
	}
	if magic.calls != 0 || sms.n != 0 {
		t.Fatal("expected no magic link or SMS for vacated tenant")
	}
}

func TestBillingCycle_IdempotentOpenDue(t *testing.T) {
	billing := &stubBilling{err: errOpenDue}
	magic := &stubMagicLink{}
	job := &BillingCycle{
		Billing:          billing,
		MagicLink:        magic,
		ErrOpenDueExists: errOpenDue,
	}
	phone := "9876543210"
	err := job.processTenant(context.Background(), domain.Tenant{
		ID:     uuid.New(),
		Phone:  &phone,
		Status: domain.TenantStatusActive,
	})
	if err != nil {
		t.Fatalf("expected open-due to be swallowed, got %v", err)
	}
	if billing.calls != 1 {
		t.Fatalf("expected one CreateRentDue call, got %d", billing.calls)
	}
	if magic.calls != 0 {
		t.Fatal("expected no magic link when open due already exists")
	}
}

func TestBillingCycle_PhoneLessSkipsNotify(t *testing.T) {
	dueID := uuid.New()
	billing := &stubBilling{due: &domain.Due{ID: dueID, Amount: 10000}}
	magic := &stubMagicLink{}
	sms := &stubSMS{}
	job := &BillingCycle{
		Billing:   billing,
		MagicLink: magic,
		SMS:       sms,
		BaseURL:   "https://pay.example.com",
	}
	err := job.processTenant(context.Background(), domain.Tenant{
		ID:     uuid.New(),
		Phone:  nil,
		Status: domain.TenantStatusActive,
	})
	if err != nil {
		t.Fatalf("processTenant: %v", err)
	}
	if billing.calls != 1 || magic.calls != 1 {
		t.Fatalf("expected due+magic link, billing=%d magic=%d", billing.calls, magic.calls)
	}
	if sms.n != 0 {
		t.Fatalf("expected phone-less to skip SMS, got %d", sms.n)
	}
}

func TestJoinURL(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"https://pay.example.com", "/p/x", "https://pay.example.com/p/x"},
		{"https://pay.example.com/", "/p/x", "https://pay.example.com/p/x"},
		{"https://pay.example.com", "p/x", "https://pay.example.com/p/x"},
		{"", "/p/x", "/p/x"},
	}
	for _, tc := range cases {
		if got := joinURL(tc.base, tc.path); got != tc.want {
			t.Fatalf("joinURL(%q,%q)=%q want %q", tc.base, tc.path, got, tc.want)
		}
	}
}
