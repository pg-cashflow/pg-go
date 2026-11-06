package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type stubOrderSettler struct {
	n           int
	orderID     string
	cfPaymentID string
	txnID       string
	amountPaise int64
	intent      *domain.PaymentIntent
	err         error
}

func (s *stubOrderSettler) SettleGatewayOrder(
	_ context.Context,
	orderID string,
	cfPaymentID string,
	txnID string,
	amountPaise int64,
	intent *domain.PaymentIntent,
	_ []byte,
) error {
	s.n++
	s.orderID = orderID
	s.cfPaymentID = cfPaymentID
	s.txnID = txnID
	s.amountPaise = amountPaise
	s.intent = intent
	return s.err
}

func TestCashfreePollWithOrderSettler_SettlesEvenIfDueClosed(t *testing.T) {
	dueID := uuid.New()
	intentID := uuid.New()
	orderID := "order-settler-closed-due"
	intents := &stubIntentStale{list: []domain.PaymentIntent{{
		ID:              intentID,
		DueID:           dueID,
		ProviderOrderID: orderID,
		Status:          domain.IntentCreated,
		AmountPaise:     50000,
	}}}
	settler := &stubOrderSettler{}
	job := &CashfreePollJob{
		Intents:      intents,
		Dues:         stubDueByID{dueID: &domain.Due{ID: dueID, Status: domain.DueStatusPaid}},
		Client:       stubCFFetch{ok: true, cfID: "cf_closed_due_1", ref: "UTR_CLOSED_1", amount: 50000},
		OrderSettler: settler,
		Now:          func() time.Time { return time.Now().UTC() },
	}

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("job.Run failed: %v", err)
	}

	if settler.n != 1 {
		t.Fatalf("expected unified OrderSettler to be called even when due is closed, got %d", settler.n)
	}
	if settler.orderID != orderID {
		t.Errorf("expected orderID %s, got %s", orderID, settler.orderID)
	}
	if settler.cfPaymentID != "cf_closed_due_1" {
		t.Errorf("expected cfPaymentID cf_closed_due_1, got %s", settler.cfPaymentID)
	}
	if settler.amountPaise != 50000 {
		t.Errorf("expected amount 50000, got %d", settler.amountPaise)
	}
	if settler.intent == nil || settler.intent.ID != intentID {
		t.Errorf("expected intent passed with ID %s", intentID)
	}
}

func TestCashfreePollWithOrderSettler_PassesFullIntentAndTransaction(t *testing.T) {
	dueID := uuid.New()
	intentID := uuid.New()
	orderID := "order-settler-multi-due"
	intents := &stubIntentStale{list: []domain.PaymentIntent{{
		ID:              intentID,
		DueID:           dueID,
		ProviderOrderID: orderID,
		Status:          domain.IntentCreated,
		AmountPaise:     125000,
	}}}
	settler := &stubOrderSettler{}
	job := &CashfreePollJob{
		Intents:      intents,
		Client:       stubCFFetch{ok: true, cfID: "cf_multi_due_99", ref: "BANK_UTR_99", amount: 125000},
		OrderSettler: settler,
		Now:          func() time.Time { return time.Now().UTC() },
	}

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("job.Run failed: %v", err)
	}

	if settler.n != 1 {
		t.Fatalf("expected OrderSettler to be called, got %d", settler.n)
	}
	if settler.txnID != "BANK_UTR_99" {
		t.Errorf("expected txnID BANK_UTR_99, got %s", settler.txnID)
	}
	if settler.amountPaise != 125000 {
		t.Errorf("expected amount 125000, got %d", settler.amountPaise)
	}
}
