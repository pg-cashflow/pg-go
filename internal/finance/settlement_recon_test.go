package finance

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockSettlementStore struct {
	items map[string]*domain.GatewaySettlement
}

func newMockSettlementStore() *mockSettlementStore {
	return &mockSettlementStore{items: make(map[string]*domain.GatewaySettlement)}
}

func (m *mockSettlementStore) UpsertSettlement(_ context.Context, s *domain.GatewaySettlement) error {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	key := s.CFSettlementID
	if s.OrderID != nil {
		key += ":" + *s.OrderID
	}
	m.items[key] = s
	return nil
}

func (m *mockSettlementStore) GetSettlement(_ context.Context, id uuid.UUID) (*domain.GatewaySettlement, error) {
	for _, v := range m.items {
		if v.ID == id {
			return v, nil
		}
	}
	return nil, ErrSettlementNotFound
}

func (m *mockSettlementStore) GetSettlementByCFID(_ context.Context, cfID, orderID, _ string) (*domain.GatewaySettlement, error) {
	key := cfID
	if orderID != "" {
		key += ":" + orderID
	}
	if v, ok := m.items[key]; ok {
		return v, nil
	}
	return nil, ErrSettlementNotFound
}

func (m *mockSettlementStore) ListSettlements(_ context.Context, _ *uuid.UUID, _ domain.SettlementFilter) ([]*domain.GatewaySettlement, int, error) {
	var list []*domain.GatewaySettlement
	for _, v := range m.items {
		list = append(list, v)
	}
	return list, len(list), nil
}

func (m *mockSettlementStore) ResolveDiscrepancy(_ context.Context, id uuid.UUID, resolvedBy uuid.UUID, notes string) error {
	for _, v := range m.items {
		if v.ID == id {
			v.ReconciliationStatus = domain.ReconManuallyReconciled
			v.ResolutionNotes = &notes
			v.ResolvedBy = &resolvedBy
			now := time.Now().UTC()
			v.ResolvedAt = &now
			return nil
		}
	}
	return ErrSettlementNotFound
}

type mockIntentStore struct {
	intents map[string]*domain.PaymentIntent
}

func (m *mockIntentStore) GetByOrderID(_ context.Context, orderID string) (*domain.PaymentIntent, error) {
	if v, ok := m.intents[orderID]; ok {
		return v, nil
	}
	return nil, nil
}

type mockDueStore struct {
	dues map[uuid.UUID]*domain.Due
}

func (m *mockDueStore) GetByID(_ context.Context, id uuid.UUID) (*domain.Due, error) {
	if v, ok := m.dues[id]; ok {
		return v, nil
	}
	return nil, nil
}

type mockPaymentStore struct {
	payments map[string]*domain.Payment
}

func (m *mockPaymentStore) GetByCFPaymentID(_ context.Context, cfID string) (*domain.Payment, error) {
	if v, ok := m.payments[cfID]; ok {
		return v, nil
	}
	return nil, nil
}

func TestSettlementReconciler_Success(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	financeSvc := NewService(st, nil)
	store := newMockSettlementStore()

	pid := uuid.New()
	dueID := uuid.New()
	intentID := uuid.New()
	orderID := "order_123"

	dues := &mockDueStore{
		dues: map[uuid.UUID]*domain.Due{
			dueID: {
				ID:         dueID,
				PropertyID: pid,
			},
		},
	}

	intents := &mockIntentStore{
		intents: map[string]*domain.PaymentIntent{
			orderID: {
				ID:              intentID,
				DueID:           dueID,
				ProviderOrderID: orderID,
				AmountPaise:     550000,
				Status:          domain.IntentPaid,
			},
		},
	}
	paymentID := uuid.New()
	cfPayID := "cf_pay_999"
	payments := &mockPaymentStore{
		payments: map[string]*domain.Payment{
			cfPayID: {
				ID:          paymentID,
				DueID:       dueID,
				CFPaymentID: &cfPayID,
				Amount:      550000,
			},
		},
	}

	reconciler := NewSettlementReconciler(store, intents, dues, payments, financeSvc)

	rec := cashfree.OrderSettlementRecord{
		CFSettlementID:     "STLM_101",
		CFPaymentID:        cfPayID,
		OrderID:            orderID,
		GrossAmountPaise:   550000,
		NetAmountPaise:     539380,
		ServiceChargePaise: 9000,
		ServiceTaxPaise:    1620,
		AdjustmentPaise:    0,
		UTR:                "UTR101",
		TransferTime:       time.Date(2026, 9, 25, 10, 0, 0, 0, time.UTC),
		Status:             "SUCCESS",
	}

	res, err := reconciler.ReconcileOrderSettlement(ctx, rec, domain.IngestionOrderFetch)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.ReconciliationStatus != domain.ReconMatched {
		t.Errorf("expected status %s, got %s", domain.ReconMatched, res.ReconciliationStatus)
	}
	if res.PropertyID == nil || *res.PropertyID != pid {
		t.Errorf("expected property id %s, got %v", pid, res.PropertyID)
	}
	if res.PaymentID == nil || *res.PaymentID != paymentID {
		t.Errorf("expected payment id %s, got %v", paymentID, res.PaymentID)
	}
	if res.JournalEntryID == nil {
		t.Errorf("expected non-nil journal entry id")
	}

	// Verify ledger accounts
	bankDr, _, _ := st.SumAccount(ctx, pid, domain.AcctBank, time.Time{}, time.Now().Add(24*time.Hour))
	if bankDr != 539380 {
		t.Errorf("expected bank Dr 539380, got %d", bankDr)
	}
	feeDr, _, _ := st.SumAccount(ctx, pid, domain.AcctPaymentProcessingExpense, time.Time{}, time.Now().Add(24*time.Hour))
	if feeDr != 10620 {
		t.Errorf("expected fee Dr 10620, got %d", feeDr)
	}
}

func TestSettlementReconciler_Discrepancies(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	financeSvc := NewService(st, nil)
	store := newMockSettlementStore()

	pid := uuid.New()
	dueID := uuid.New()
	orderID := "order_valid"

	dues := &mockDueStore{
		dues: map[uuid.UUID]*domain.Due{
			dueID: {
				ID:         dueID,
				PropertyID: pid,
			},
		},
	}

	intents := &mockIntentStore{
		intents: map[string]*domain.PaymentIntent{
			orderID: {
				ID:              uuid.New(),
				DueID:           dueID,
				ProviderOrderID: orderID,
				AmountPaise:     500000,
				Status:          domain.IntentPaid,
			},
		},
	}
	reconciler := NewSettlementReconciler(store, intents, dues, &mockPaymentStore{}, financeSvc)

	// 1. Missing Intent
	res1, err := reconciler.ReconcileOrderSettlement(ctx, cashfree.OrderSettlementRecord{
		CFSettlementID:     "S_01",
		OrderID:            "order_ghost",
		GrossAmountPaise:   100000,
		NetAmountPaise:     98000,
		ServiceChargePaise: 2000,
	}, domain.IngestionWebhook)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res1.ReconciliationStatus != domain.ReconUnmatched {
		t.Errorf("expected unmatched, got %s", res1.ReconciliationStatus)
	}
	if res1.JournalEntryID != nil {
		t.Errorf("expected nil journal entry id on unmatched")
	}

	// 2. Arithmetic Imbalance
	res2, err := reconciler.ReconcileOrderSettlement(ctx, cashfree.OrderSettlementRecord{
		CFSettlementID:     "S_02",
		OrderID:            orderID,
		GrossAmountPaise:   500000,
		NetAmountPaise:     480000,
		ServiceChargePaise: 10000,
		// Missing 10000 paise
	}, domain.IngestionWebhook)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res2.ReconciliationStatus != domain.ReconDiscrepancy {
		t.Errorf("expected discrepancy on arithmetic imbalance, got %s", res2.ReconciliationStatus)
	}

	// 3. Amount Mismatch with Intent
	res3, err := reconciler.ReconcileOrderSettlement(ctx, cashfree.OrderSettlementRecord{
		CFSettlementID:     "S_03",
		OrderID:            orderID,
		GrossAmountPaise:   400000, // Intent is 500000
		NetAmountPaise:     390000,
		ServiceChargePaise: 10000,
	}, domain.IngestionWebhook)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res3.ReconciliationStatus != domain.ReconDiscrepancy {
		t.Errorf("expected discrepancy on intent mismatch, got %s", res3.ReconciliationStatus)
	}
}
