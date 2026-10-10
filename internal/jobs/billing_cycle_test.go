package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

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

type stubRecurringExpenses struct {
	called bool
	asOf   time.Time
}

func (s *stubRecurringExpenses) ProcessRecurringExpenses(_ context.Context, asOf time.Time) error {
	s.called = true
	s.asOf = asOf
	return nil
}

type stubTenantLister struct {
	tenants []domain.Tenant
}

func (s stubTenantLister) ListActiveByDueDay(_ context.Context, _ int, _ *uuid.UUID) ([]domain.Tenant, error) {
	return s.tenants, nil
}

func TestBillingCycle_RecurringExpensesHook(t *testing.T) {
	rec := &stubRecurringExpenses{}
	job := &BillingCycle{
		Tenants:           stubTenantLister{},
		Billing:           &stubBilling{},
		RecurringExpenses: rec,
	}

	err := job.Run(context.Background())
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}
	if !rec.called {
		t.Errorf("expected RecurringExpenses hook to be called during Run, but it was not")
	}
	if rec.asOf.IsZero() {
		t.Errorf("expected asOf time to be populated, got zero time")
	}
}

type stubActiveTenantLister struct {
	tenants []domain.Tenant
}

func (s stubActiveTenantLister) ListActiveByDueDay(_ context.Context, _ int, _ *uuid.UUID) ([]domain.Tenant, error) {
	return s.tenants, nil
}

func TestGenerateMonthlyRentDues(t *testing.T) {
	ctx := context.Background()
	t1 := domain.Tenant{ID: uuid.New(), Status: domain.TenantStatusActive}
	t2 := domain.Tenant{ID: uuid.New(), Status: domain.TenantStatusActive}
	t3 := domain.Tenant{ID: uuid.New(), Status: domain.TenantStatusActive}
	t4 := domain.Tenant{ID: uuid.New(), Status: domain.TenantStatusVacated}

	calls := 0
	billingStub := &customStubBilling{
		createFunc: func(ctx context.Context, tn *domain.Tenant) (*domain.Due, error) {
			calls++
			if tn.ID == t1.ID {
				return &domain.Due{ID: uuid.New(), Amount: 500000}, nil
			}
			if tn.ID == t2.ID {
				return nil, errOpenDue
			}
			return nil, errors.New("database disk full")
		},
	}

	job := &BillingCycle{
		Tenants:          stubActiveTenantLister{tenants: []domain.Tenant{t1, t2, t3, t4}},
		Billing:          billingStub,
		ErrOpenDueExists: errOpenDue,
	}

	report, err := job.GenerateMonthlyRentDues(ctx, time.Now())
	if err == nil {
		t.Fatal("expected error due to t3 failure")
	}
	if report.TenantsProcessed != 3 { // t1, t2, t3 (t4 was skipped because vacated)
		t.Fatalf("expected 3 tenants processed, got %d", report.TenantsProcessed)
	}
	if report.DuesCreated != 1 {
		t.Fatalf("expected 1 due created, got %d", report.DuesCreated)
	}
	if report.ExistingDuesSkipped != 1 {
		t.Fatalf("expected 1 due skipped, got %d", report.ExistingDuesSkipped)
	}
	if report.Errors != 1 {
		t.Fatalf("expected 1 error, got %d", report.Errors)
	}
}

type customStubBilling struct {
	createFunc func(ctx context.Context, tenant *domain.Tenant) (*domain.Due, error)
}

func (c *customStubBilling) CreateRentDue(ctx context.Context, tenant *domain.Tenant) (*domain.Due, error) {
	return c.createFunc(ctx, tenant)
}
