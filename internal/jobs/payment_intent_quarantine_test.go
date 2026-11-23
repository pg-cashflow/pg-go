package jobs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type quarantineStoreStub struct {
	items      []domain.PaymentIntentQuarantine
	resolved   []int64
	attempts   []quarantineAttempt
	listErr    error
	resolveErr error
	recordErr  error
}

type quarantineAttempt struct {
	id             int64
	status         string
	providerStatus string
	cause          error
	next           time.Time
}

func (s *quarantineStoreStub) ListReady(context.Context, int) ([]domain.PaymentIntentQuarantine, error) {
	return s.items, s.listErr
}
func (s *quarantineStoreStub) RecordAttempt(_ context.Context, id int64, status, providerStatus string, cause error, next time.Time) error {
	s.attempts = append(s.attempts, quarantineAttempt{id: id, status: status, providerStatus: providerStatus, cause: cause, next: next})
	return s.recordErr
}
func (s *quarantineStoreStub) MarkResolved(_ context.Context, id int64, _ string) error {
	s.resolved = append(s.resolved, id)
	return s.resolveErr
}

type quarantineGatewayStub struct {
	payments       []paymentLookup
	orders         []*cashfree.OrderDetails
	terminateOrder *cashfree.OrderDetails
	paymentCalls   int
	getCalls       int
	terminateCalls int
	getErr         error
	terminateErr   error
}

type paymentLookup struct {
	cfID   string
	bank   string
	amount int64
	ok     bool
	err    error
}

func (g *quarantineGatewayStub) FetchSuccessfulPayment(context.Context, string) (string, string, int64, bool, error) {
	idx := g.paymentCalls
	g.paymentCalls++
	if idx >= len(g.payments) {
		return "", "", 0, false, nil
	}
	p := g.payments[idx]
	return p.cfID, p.bank, p.amount, p.ok, p.err
}
func (g *quarantineGatewayStub) GetOrder(context.Context, string) (*cashfree.OrderDetails, error) {
	idx := g.getCalls
	g.getCalls++
	if g.getErr != nil {
		return nil, g.getErr
	}
	if idx >= len(g.orders) {
		return nil, nil
	}
	return g.orders[idx], nil
}
func (g *quarantineGatewayStub) TerminateOrder(context.Context, string) (*cashfree.OrderDetails, error) {
	g.terminateCalls++
	return g.terminateOrder, g.terminateErr
}

type quarantineSettlerStub struct {
	calls  int
	order  string
	cfID   string
	amount int64
	intent *domain.PaymentIntent
	err    error
}

func (s *quarantineSettlerStub) SettleGatewayOrder(_ context.Context, order, cfID, _ string, amount int64, intent *domain.PaymentIntent, _ []byte) error {
	s.calls++
	s.order, s.cfID, s.amount, s.intent = order, cfID, amount, intent
	return s.err
}

func newQuarantineItem() domain.PaymentIntentQuarantine {
	intentID := uuid.New()
	dueID := uuid.New()
	return domain.PaymentIntentQuarantine{
		ID:              7,
		PaymentIntentID: intentID,
		DueID:           dueID,
		PropertyID:      uuid.New(),
		DueIDs:          []uuid.UUID{dueID},
		ProviderOrderID: "old-cf-order",
		AmountPaise:     12500,
		AttemptCount:    0,
		Intent: domain.PaymentIntent{
			ID:              intentID,
			DueID:           dueID,
			Provider:        "cashfree",
			ProviderOrderID: "old-cf-order",
			AmountPaise:     12500,
			Status:          domain.IntentSuperseded,
		},
	}
}

func TestPaymentIntentQuarantineWorkerKeepsDueLockedWhenTerminationIsRequested(t *testing.T) {
	store := &quarantineStoreStub{items: []domain.PaymentIntentQuarantine{newQuarantineItem()}}
	gateway := &quarantineGatewayStub{
		orders:         []*cashfree.OrderDetails{{OrderID: "old-cf-order", OrderStatus: "ACTIVE", OrderAmountPaise: 12500}},
		terminateOrder: &cashfree.OrderDetails{OrderID: "old-cf-order", OrderStatus: "TERMINATION_REQUESTED", OrderAmountPaise: 12500},
	}
	settler := &quarantineSettlerStub{}
	job := &PaymentIntentQuarantineJob{Store: store, Gateway: gateway, Settler: settler, Now: func() time.Time { return time.Date(2026, 11, 22, 10, 0, 0, 0, time.UTC) }}

	if err := job.Run(context.Background(), 10); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}
	if len(store.resolved) != 0 {
		t.Fatalf("termination requested is not terminal; resolved IDs: %v", store.resolved)
	}
	if gateway.terminateCalls != 1 || len(store.attempts) != 1 || store.attempts[0].status != "termination_requested" {
		t.Fatalf("termination did not remain quarantined: terminations=%d attempts=%+v", gateway.terminateCalls, store.attempts)
	}
}

func TestPaymentIntentQuarantineWorkerRequiresTerminalConfirmationAcrossWorkerPasses(t *testing.T) {
	item := newQuarantineItem()
	store := &quarantineStoreStub{items: []domain.PaymentIntentQuarantine{item}}
	gateway := &quarantineGatewayStub{
		orders:   []*cashfree.OrderDetails{{OrderID: "old-cf-order", OrderStatus: "TERMINATED", OrderAmountPaise: 12500}, {OrderID: "old-cf-order", OrderStatus: "TERMINATED", OrderAmountPaise: 12500}},
		payments: []paymentLookup{{}, {}, {}, {}},
	}
	job := &PaymentIntentQuarantineJob{Store: store, Gateway: gateway, Settler: &quarantineSettlerStub{}}

	if err := job.Run(context.Background(), 10); err != nil {
		t.Fatalf("first Run returned unexpected error: %v", err)
	}
	if len(store.resolved) != 0 || len(store.attempts) != 1 {
		t.Fatalf("first terminal observation must remain quarantined; resolved=%v attempts=%+v", store.resolved, store.attempts)
	}
	item.Status = store.attempts[0].status
	providerStatus := store.attempts[0].providerStatus
	item.LastProviderStatus = &providerStatus
	store.items[0] = item

	if err := job.Run(context.Background(), 10); err != nil {
		t.Fatalf("second Run returned unexpected error: %v", err)
	}
	if len(store.resolved) != 1 || store.resolved[0] != 7 {
		t.Fatalf("expected repeated terminal state plus final empty payment check to resolve, got %v", store.resolved)
	}
	if gateway.paymentCalls != 4 {
		t.Fatalf("expected two payment lookups on each pass, got %d", gateway.paymentCalls)
	}
}

func TestPaymentIntentQuarantineWorkerSettlesLateCaptureBeforeTermination(t *testing.T) {
	store := &quarantineStoreStub{items: []domain.PaymentIntentQuarantine{newQuarantineItem()}}
	gateway := &quarantineGatewayStub{
		payments: []paymentLookup{{cfID: "cf-late", bank: "UTR-1", amount: 12500, ok: true}},
		orders:   []*cashfree.OrderDetails{{OrderID: "old-cf-order", OrderStatus: "PAID", OrderAmountPaise: 12500}},
	}
	settler := &quarantineSettlerStub{}
	job := &PaymentIntentQuarantineJob{Store: store, Gateway: gateway, Settler: settler}

	if err := job.Run(context.Background(), 10); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}
	if settler.calls != 1 || settler.cfID != "cf-late" || settler.amount != 12500 {
		t.Fatalf("late payment not settled: %+v", settler)
	}
	if len(store.resolved) != 0 || len(store.attempts) != 1 || store.attempts[0].status != "needs_review" || store.attempts[0].providerStatus != "PAID" || gateway.getCalls != 1 || gateway.terminateCalls != 0 {
		t.Fatalf("late capture must remain quarantined for owner resolution after provider PAID confirmation; resolved=%v attempts=%+v gets=%d terminations=%d", store.resolved, store.attempts, gateway.getCalls, gateway.terminateCalls)
	}
}

func TestPaymentIntentQuarantineWorkerDoesNotReleasePaidCaptureWhileOrderRemainsActive(t *testing.T) {
	store := &quarantineStoreStub{items: []domain.PaymentIntentQuarantine{newQuarantineItem()}}
	gateway := &quarantineGatewayStub{
		payments:       []paymentLookup{{cfID: "cf-late", bank: "UTR-2", amount: 12500, ok: true}},
		orders:         []*cashfree.OrderDetails{{OrderID: "old-cf-order", OrderStatus: "ACTIVE", OrderAmountPaise: 12500}},
		terminateOrder: &cashfree.OrderDetails{OrderID: "old-cf-order", OrderStatus: "TERMINATION_REQUESTED", OrderAmountPaise: 12500},
	}
	settler := &quarantineSettlerStub{}
	job := &PaymentIntentQuarantineJob{Store: store, Gateway: gateway, Settler: settler}

	if err := job.Run(context.Background(), 10); err != nil {
		t.Fatalf("Run returned unexpected error: %v", err)
	}
	if settler.calls != 1 || gateway.terminateCalls != 1 {
		t.Fatalf("late capture should be settled and active order termination requested; settle=%d terminate=%d", settler.calls, gateway.terminateCalls)
	}
	if len(store.resolved) != 0 || len(store.attempts) != 1 || store.attempts[0].status != "needs_review" || store.attempts[0].providerStatus != "TERMINATION_REQUESTED" {
		t.Fatalf("active provider order must remain quarantined; resolved=%v attempts=%+v", store.resolved, store.attempts)
	}
}

func TestPaymentIntentQuarantineWorkerDoesNotResolvePaidOrderWithoutPaymentRecord(t *testing.T) {
	store := &quarantineStoreStub{items: []domain.PaymentIntentQuarantine{newQuarantineItem()}}
	gateway := &quarantineGatewayStub{
		orders:   []*cashfree.OrderDetails{{OrderID: "old-cf-order", OrderStatus: "PAID", OrderAmountPaise: 12500}},
		payments: []paymentLookup{{}, {}},
	}
	job := &PaymentIntentQuarantineJob{Store: store, Gateway: gateway, Settler: &quarantineSettlerStub{}}

	if err := job.Run(context.Background(), 10); err == nil {
		t.Fatal("expected PAID order without payment record to remain in review")
	}
	if len(store.resolved) != 0 || len(store.attempts) != 1 || store.attempts[0].status != "needs_review" {
		t.Fatalf("a PAID order without a successful payment record must remain blocked; resolved=%v attempts=%+v", store.resolved, store.attempts)
	}
}

func TestPaymentIntentQuarantineWorkerRetainsQueueWhenSettlementFails(t *testing.T) {
	store := &quarantineStoreStub{items: []domain.PaymentIntentQuarantine{newQuarantineItem()}}
	gateway := &quarantineGatewayStub{payments: []paymentLookup{{cfID: "cf-late", amount: 12500, ok: true}}}
	settler := &quarantineSettlerStub{err: errors.New("database unavailable")}
	job := &PaymentIntentQuarantineJob{Store: store, Gateway: gateway, Settler: settler}

	if err := job.Run(context.Background(), 10); err == nil {
		t.Fatal("expected settlement error")
	}
	if len(store.resolved) != 0 || len(store.attempts) != 1 {
		t.Fatalf("failed settlement must remain quarantined; resolved=%v attempts=%+v", store.resolved, store.attempts)
	}
}

func TestPaymentIntentQuarantineWorkerRecordsRepeatedPaidTerminalObservations(t *testing.T) {
	item := newQuarantineItem()
	store := &quarantineStoreStub{items: []domain.PaymentIntentQuarantine{item}}
	gateway := &quarantineGatewayStub{
		payments: []paymentLookup{
			{cfID: "cf-late-repeat", bank: "UTR-REPEAT", amount: 12500, ok: true},
			{cfID: "cf-late-repeat", bank: "UTR-REPEAT", amount: 12500, ok: true},
		},
		orders: []*cashfree.OrderDetails{
			{OrderID: "old-cf-order", OrderStatus: "PAID", OrderAmountPaise: 12500},
			{OrderID: "old-cf-order", OrderStatus: "PAID", OrderAmountPaise: 12500},
		},
	}
	job := &PaymentIntentQuarantineJob{Store: store, Gateway: gateway, Settler: &quarantineSettlerStub{}}

	if err := job.Run(context.Background(), 10); err != nil {
		t.Fatalf("first paid observation returned unexpected error: %v", err)
	}
	item.Status = store.attempts[0].status
	providerStatus := store.attempts[0].providerStatus
	item.LastProviderStatus = &providerStatus
	store.items[0] = item

	if err := job.Run(context.Background(), 10); err != nil {
		t.Fatalf("second paid observation returned unexpected error: %v", err)
	}
	if len(store.resolved) != 0 || len(store.attempts) != 2 {
		t.Fatalf("paid order must remain quarantined for owner resolution; resolved=%v attempts=%+v", store.resolved, store.attempts)
	}
	if store.attempts[0].providerStatus != "PAID" || store.attempts[1].providerStatus != "PAID" {
		t.Fatalf("expected two matching PAID observations, got %+v", store.attempts)
	}
}
