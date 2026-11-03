package payment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockGatewayPaymentRepo struct {
	processedEvents map[string]bool
	unmatchedCount  int
	payments        []*domain.Payment
}

func newMockGatewayPaymentRepo() *mockGatewayPaymentRepo {
	return &mockGatewayPaymentRepo{
		processedEvents: make(map[string]bool),
	}
}

func (m *mockGatewayPaymentRepo) RecordProcessedEvent(_ context.Context, provider, eventType, referenceID, payloadHash string) (bool, error) {
	key := provider + ":" + eventType + ":" + referenceID
	if m.processedEvents[key] {
		return false, nil
	}
	m.processedEvents[key] = true
	return true, nil
}

func (m *mockGatewayPaymentRepo) RecordUnmatchedReceipt(_ context.Context, orderID, cfPaymentID string, intentID *uuid.UUID, amountPaise int64, reason string, payload []byte) error {
	m.unmatchedCount++
	return nil
}

func (m *mockGatewayPaymentRepo) Create(_ context.Context, p *domain.Payment) error {
	m.payments = append(m.payments, p)
	return nil
}

func (m *mockGatewayPaymentRepo) CreateAllocation(_ context.Context, paymentID, dueID uuid.UUID, amountPaise int64) error {
	return nil
}

type mockPaymentIntentRepo struct {
	intent   *domain.PaymentIntent
	snapshot []domain.PaymentIntentDue
	paidID   uuid.UUID
	paidCFID string
}

func (m *mockPaymentIntentRepo) GetByOrderID(_ context.Context, orderID string) (*domain.PaymentIntent, error) {
	if m.intent != nil && m.intent.ProviderOrderID == orderID {
		return m.intent, nil
	}
	return nil, errors.New("intent not found")
}

func (m *mockPaymentIntentRepo) GetByCFPaymentID(_ context.Context, cfPaymentID string) (*domain.PaymentIntent, error) {
	return m.intent, nil
}

func (m *mockPaymentIntentRepo) GetDuesSnapshot(_ context.Context, intentID uuid.UUID) ([]domain.PaymentIntentDue, error) {
	return m.snapshot, nil
}

func (m *mockPaymentIntentRepo) MarkPaid(_ context.Context, id uuid.UUID, cfPaymentID string) error {
	m.paidID = id
	m.paidCFID = cfPaymentID
	return nil
}

type stubDueRepo struct {
	dues map[uuid.UUID]*domain.Due
}

func (s *stubDueRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Due, error) {
	return s.dues[id], nil
}
func (s *stubDueRepo) GetByDueCode(_ context.Context, _ string) (*domain.Due, error) { return nil, nil }
func (s *stubDueRepo) Update(_ context.Context, d *domain.Due) error {
	s.dues[d.ID] = d
	return nil
}
func (s *stubDueRepo) ListByTenant(_ context.Context, _ uuid.UUID) ([]domain.Due, error) {
	return nil, nil
}
func (s *stubDueRepo) FindByAmountAndDateWindow(_ context.Context, _ uuid.UUID, _ int64, _, _ time.Time) ([]domain.Due, error) {
	return nil, nil
}

type stubPaymentRepo struct {
	payments map[string]*domain.Payment
}

func (s *stubPaymentRepo) Create(_ context.Context, p *domain.Payment) error {
	if p.UPITxnID != nil {
		s.payments[*p.UPITxnID] = p
	}
	return nil
}
func (s *stubPaymentRepo) GetByID(_ context.Context, _ uuid.UUID) (*domain.Payment, error) {
	return nil, nil
}
func (s *stubPaymentRepo) GetByUPITxnID(_ context.Context, txnID string) (*domain.Payment, error) {
	return s.payments[txnID], nil
}

type stubTenantRepo struct {
	tenants map[uuid.UUID]*domain.Tenant
}

func (s *stubTenantRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
	return s.tenants[id], nil
}
func (s *stubTenantRepo) Update(_ context.Context, t *domain.Tenant) error {
	s.tenants[t.ID] = t
	return nil
}
func (s *stubTenantRepo) AddCredit(_ context.Context, id uuid.UUID, delta int64) error {
	if t, ok := s.tenants[id]; ok {
		t.CreditBalancePaise += delta
	}
	return nil
}

type stubPublisher struct{}

func (s stubPublisher) Publish(_ context.Context, _ domain.Event) error { return nil }

func TestOrderSettlementService_Deduplication(t *testing.T) {
	gwRepo := newMockGatewayPaymentRepo()
	intentID := uuid.New()
	intentRepo := &mockPaymentIntentRepo{
		intent: &domain.PaymentIntent{
			ID:              intentID,
			ProviderOrderID: "order-123",
			AmountPaise:     10000,
			Status:          domain.IntentCreated,
		},
	}
	svc := NewOrderSettlementService(OrderSettlementConfig{
		GatewayPaymentRepo: gwRepo,
		IntentStore:        intentRepo,
	})

	ctx := context.Background()
	// First settlement call succeeds
	err := svc.SettleGatewayOrder(ctx, "order-123", "cf_pay_1", "txn_1", 10000, nil, nil)
	if err != nil {
		t.Fatalf("first settlement failed: %v", err)
	}

	// Second settlement call with identical cfPaymentID is deduplicated (returns nil without duplicate work)
	err = svc.SettleGatewayOrder(ctx, "order-123", "cf_pay_1", "txn_1", 10000, nil, nil)
	if err != nil {
		t.Fatalf("duplicate settlement failed: %v", err)
	}
}

func TestOrderSettlementService_AmountMismatch(t *testing.T) {
	gwRepo := newMockGatewayPaymentRepo()
	intentID := uuid.New()
	intentRepo := &mockPaymentIntentRepo{
		intent: &domain.PaymentIntent{
			ID:              intentID,
			ProviderOrderID: "order-mismatch",
			AmountPaise:     50000,
			Status:          domain.IntentCreated,
		},
	}
	svc := NewOrderSettlementService(OrderSettlementConfig{
		GatewayPaymentRepo: gwRepo,
		IntentStore:        intentRepo,
	})

	ctx := context.Background()
	err := svc.SettleGatewayOrder(ctx, "order-mismatch", "cf_pay_err", "txn_err", 40000, nil, nil)
	if err == nil {
		t.Fatal("expected error on amount mismatch, got nil")
	}
	if gwRepo.unmatchedCount != 1 {
		t.Errorf("expected 1 unmatched receipt recorded, got %d", gwRepo.unmatchedCount)
	}
}

func TestOrderSettlementService_MultiDueSnapshot(t *testing.T) {
	dueID1 := uuid.New()
	dueID2 := uuid.New()
	tenantID := uuid.New()
	propID := uuid.New()

	due1 := &domain.Due{ID: dueID1, TenantID: tenantID, PropertyID: propID, Amount: 3000, Status: domain.DueStatusPending}
	due2 := &domain.Due{ID: dueID2, TenantID: tenantID, PropertyID: propID, Amount: 4000, Status: domain.DueStatusPending}

	duesMap := map[uuid.UUID]*domain.Due{dueID1: due1, dueID2: due2}
	dueRepo := &stubDueRepo{dues: duesMap}
	payRepo := &stubPaymentRepo{payments: make(map[string]*domain.Payment)}
	tenantRepo := &stubTenantRepo{tenants: map[uuid.UUID]*domain.Tenant{tenantID: {ID: tenantID}}}

	paySvc := NewService(dueRepo, payRepo, tenantRepo, nil, stubPublisher{})

	intentID := uuid.New()
	intentRepo := &mockPaymentIntentRepo{
		intent: &domain.PaymentIntent{
			ID:              intentID,
			DueID:           dueID1,
			ProviderOrderID: "order-multi",
			AmountPaise:     7000,
			Status:          domain.IntentCreated,
		},
		snapshot: []domain.PaymentIntentDue{
			{DueID: dueID1, AmountPaise: 3000},
			{DueID: dueID2, AmountPaise: 4000},
		},
	}
	gwRepo := newMockGatewayPaymentRepo()

	svc := NewOrderSettlementService(OrderSettlementConfig{
		GatewayPaymentRepo: gwRepo,
		IntentStore:        intentRepo,
		Payments:           paySvc,
	})

	ctx := context.Background()
	err := svc.SettleGatewayOrder(ctx, "order-multi", "cf_multi_pay_1", "txn_multi_1", 7000, nil, nil)
	if err != nil {
		t.Fatalf("multi-due settlement failed: %v", err)
	}

	if due1.Status != domain.DueStatusPaid {
		t.Errorf("expected due1 status paid, got %s", due1.Status)
	}
	if due2.Status != domain.DueStatusPaid {
		t.Errorf("expected due2 status paid, got %s", due2.Status)
	}
	if intentRepo.paidCFID != "cf_multi_pay_1" {
		t.Errorf("expected intent marked paid with cf_multi_pay_1, got %s", intentRepo.paidCFID)
	}
}
