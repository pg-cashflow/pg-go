package payment

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/events"
)

type stubPayments struct {
	created []*domain.Payment
	byTxn   map[string]*domain.Payment
}

func (s *stubPayments) Create(_ context.Context, p *domain.Payment) error {
	if p.ID == uuid.Nil {
		p.ID = uuid.New()
	}
	cp := *p
	s.created = append(s.created, &cp)
	if p.UPITxnID != nil {
		if s.byTxn == nil {
			s.byTxn = map[string]*domain.Payment{}
		}
		s.byTxn[*p.UPITxnID] = &cp
	}
	return nil
}

func (s *stubPayments) GetByUPITxnID(_ context.Context, txnID string) (*domain.Payment, error) {
	if s.byTxn == nil {
		return nil, pgx.ErrNoRows
	}
	p, ok := s.byTxn[txnID]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	cp := *p
	return &cp, nil
}

func (s *stubPayments) RecordWebhookEvent(_ context.Context, dedupKey, _, _ string) (bool, error) {
	if s.byTxn == nil {
		s.byTxn = make(map[string]*domain.Payment)
	}
	if s.created == nil {
		s.created = []*domain.Payment{}
	}
	// We use byTxn or a dedup map. Let's track dedup keys in a special marker or map.
	if s.byTxn["__dedup__:"+dedupKey] != nil {
		return false, nil
	}
	s.byTxn["__dedup__:"+dedupKey] = &domain.Payment{}
	return true, nil
}

type stubTenants struct {
	byID map[uuid.UUID]*domain.Tenant
}

func (s *stubTenants) GetByID(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
	t, ok := s.byID[id]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	cp := *t
	return &cp, nil
}

func (s *stubTenants) Update(_ context.Context, t *domain.Tenant) error {
	cp := *t
	s.byID[t.ID] = &cp
	return nil
}

type recordingPublisher struct {
	events []domain.Event
}

func (p *recordingPublisher) Publish(_ context.Context, e domain.Event) error {
	p.events = append(p.events, e)
	return nil
}

var _ events.Publisher = (*recordingPublisher)(nil)

func TestMarkCashPaidExactOK(t *testing.T) {
	dueID := uuid.New()
	tenantID := uuid.New()
	propID := uuid.New()
	due := &domain.Due{
		ID: dueID, TenantID: tenantID, PropertyID: propID,
		DueCode: "CASH01", Amount: 15000, OriginalAmount: 15000,
		Status: domain.DueStatusPending, DueDate: time.Now().UTC(),
	}
	dues := &stubDues{byID: map[uuid.UUID]*domain.Due{dueID: due}}
	pays := &stubPayments{}
	tenants := &stubTenants{byID: map[uuid.UUID]*domain.Tenant{
		tenantID: {ID: tenantID, PropertyID: propID},
	}}
	pub := &recordingPublisher{}
	svc := NewService(dues, pays, tenants, nil, pub)
	recorder := uuid.New()

	p, err := svc.MarkCashPaid(context.Background(), dueID, 15000, recorder, "gate")
	if err != nil {
		t.Fatal(err)
	}
	if p.MatchedBy != domain.MatchedByCash {
		t.Fatalf("matched_by=%s", p.MatchedBy)
	}
	if p.RecordedBy == nil || *p.RecordedBy != recorder {
		t.Fatal("recorded_by not set")
	}
	updated, _ := dues.GetByID(context.Background(), dueID)
	if updated.Status != domain.DueStatusPaid || updated.Amount != 0 {
		t.Fatalf("due not fully paid: %#v", updated)
	}
	var sawCash, sawPaid bool
	for _, e := range pub.events {
		if e.EventType == domain.EvtCashPaymentRecorded {
			sawCash = true
		}
		if e.EventType == domain.EvtDuePaidOnTime || e.EventType == domain.EvtDuePaidLate {
			sawPaid = true
		}
	}
	if !sawCash || !sawPaid {
		t.Fatalf("missing events: cash=%v paid=%v events=%v", sawCash, sawPaid, pub.events)
	}
}

func TestMarkCashPaidUnderpayRejected(t *testing.T) {
	dueID := uuid.New()
	due := &domain.Due{
		ID: dueID, TenantID: uuid.New(), PropertyID: uuid.New(),
		DueCode: "CASH02", Amount: 15000, OriginalAmount: 15000,
		Status: domain.DueStatusPending,
	}
	dues := &stubDues{byID: map[uuid.UUID]*domain.Due{dueID: due}}
	svc := NewService(dues, &stubPayments{}, &stubTenants{byID: map[uuid.UUID]*domain.Tenant{}}, nil, &recordingPublisher{})
	_, err := svc.MarkCashPaid(context.Background(), dueID, 14999, uuid.New(), "")
	if !errors.Is(err, ErrCashPartialNotAllowed) {
		t.Fatalf("want ErrCashPartialNotAllowed, got %v", err)
	}
	updated, _ := dues.GetByID(context.Background(), dueID)
	if updated.Status != domain.DueStatusPending || updated.Amount != 15000 {
		t.Fatalf("due should be unchanged: %#v", updated)
	}
}

func TestMarkCashPaidOverpayRejected(t *testing.T) {
	dueID := uuid.New()
	due := &domain.Due{
		ID: dueID, TenantID: uuid.New(), PropertyID: uuid.New(),
		DueCode: "CASH03", Amount: 1000, OriginalAmount: 5000,
		Status: domain.DueStatusPartial,
	}
	dues := &stubDues{byID: map[uuid.UUID]*domain.Due{dueID: due}}
	svc := NewService(dues, &stubPayments{}, &stubTenants{byID: map[uuid.UUID]*domain.Tenant{}}, nil, &recordingPublisher{})
	_, err := svc.MarkCashPaid(context.Background(), dueID, 1001, uuid.New(), "")
	if !errors.Is(err, ErrCashPartialNotAllowed) {
		t.Fatalf("want ErrCashPartialNotAllowed, got %v", err)
	}
}

func TestDualPayPartialThenCashCloses(t *testing.T) {
	dueID := uuid.New()
	tenantID := uuid.New()
	propID := uuid.New()
	due := &domain.Due{
		ID: dueID, TenantID: tenantID, PropertyID: propID,
		DueCode: "DUAL01", Amount: 10000, OriginalAmount: 10000,
		Status: domain.DueStatusPending, DueDate: time.Now().UTC(),
	}
	dues := &stubDues{byID: map[uuid.UUID]*domain.Due{dueID: due}}
	pays := &stubPayments{}
	tenants := &stubTenants{byID: map[uuid.UUID]*domain.Tenant{
		tenantID: {ID: tenantID, PropertyID: propID},
	}}
	pub := &recordingPublisher{}
	svc := NewService(dues, pays, tenants, nil, pub)

	txn := "UTR1"
	p1, err := svc.settleMatched(context.Background(), dueID, 4000, domain.MatchedByDueCode, &txn, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Amount != 4000 {
		t.Fatalf("first pay amount=%d", p1.Amount)
	}
	mid, _ := dues.GetByID(context.Background(), dueID)
	if mid.Status != domain.DueStatusPartial || mid.Amount != 6000 {
		t.Fatalf("after partial: status=%s amount=%d", mid.Status, mid.Amount)
	}

	p2, err := svc.MarkCashPaid(context.Background(), dueID, 6000, uuid.New(), "gate close")
	if err != nil {
		t.Fatal(err)
	}
	if p2.MatchedBy != domain.MatchedByCash {
		t.Fatalf("second matched_by=%s", p2.MatchedBy)
	}
	if len(pays.created) != 2 {
		t.Fatalf("want 2 payment rows, got %d", len(pays.created))
	}
	final, _ := dues.GetByID(context.Background(), dueID)
	if final.Status != domain.DueStatusPaid || final.Amount != 0 {
		t.Fatalf("want fully paid, got %#v", final)
	}
}

func TestGatewaySettleCashfree(t *testing.T) {
	dueID := uuid.New()
	tenantID := uuid.New()
	propID := uuid.New()
	due := &domain.Due{
		ID: dueID, TenantID: tenantID, PropertyID: propID,
		DueCode: "CF001A", Amount: 5000, OriginalAmount: 5000,
		Status: domain.DueStatusPending, DueDate: time.Now().UTC(),
	}
	dues := &stubDues{byID: map[uuid.UUID]*domain.Due{dueID: due}}
	pays := &stubPayments{}
	svc := NewService(dues, pays, &stubTenants{byID: map[uuid.UUID]*domain.Tenant{
		tenantID: {ID: tenantID, PropertyID: propID},
	}}, nil, &recordingPublisher{})

	p, err := svc.GatewaySettle(context.Background(), dueID, 5000, "UTR-CF-1")
	if err != nil {
		t.Fatal(err)
	}
	if p.MatchedBy != domain.MatchedByCashfree {
		t.Fatalf("matched_by=%s", p.MatchedBy)
	}
	again, err := svc.GatewaySettle(context.Background(), dueID, 5000, "UTR-CF-1")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != p.ID {
		t.Fatal("duplicate UTR must be idempotent")
	}
}

func TestGatewaySettleRequiresTxnID(t *testing.T) {
	svc := NewService(&stubDues{}, &stubPayments{}, &stubTenants{}, nil, &recordingPublisher{})
	if _, err := svc.GatewaySettle(context.Background(), uuid.New(), 5000, ""); err != ErrEmptyTxnID {
		t.Fatalf("got %v", err)
	}
}

func TestGatewaySettle_DueAlreadyPaid_ConvertsToTenantCredit(t *testing.T) {
	dueID := uuid.New()
	tenantID := uuid.New()
	propID := uuid.New()
	due := &domain.Due{
		ID: dueID, TenantID: tenantID, PropertyID: propID,
		DueCode: "CF002B", Amount: 0, OriginalAmount: 8000,
		Status: domain.DueStatusPaid, DueDate: time.Now().UTC(),
	}
	dues := &stubDues{byID: map[uuid.UUID]*domain.Due{dueID: due}}
	pays := &stubPayments{}
	tenants := &stubTenants{byID: map[uuid.UUID]*domain.Tenant{
		tenantID: {ID: tenantID, PropertyID: propID, CreditBalancePaise: 0},
	}}
	pub := &recordingPublisher{}
	svc := NewService(dues, pays, tenants, nil, pub)

	p, err := svc.GatewaySettle(context.Background(), dueID, 8000, "UTR-CF-OVERPAY")
	if err != nil {
		t.Fatalf("unexpected error on gateway settle for paid due: %v", err)
	}
	if p == nil {
		t.Fatal("expected payment to be persisted, got nil")
	}
	if p.MatchedBy != domain.MatchedByCashfree {
		t.Fatalf("expected matched_by=cashfree, got %s", p.MatchedBy)
	}

	// Verify tenant credit was updated
	updatedTenant, _ := tenants.GetByID(context.Background(), tenantID)
	if updatedTenant.CreditBalancePaise != 8000 {
		t.Fatalf("expected credit_balance_paise=8000, got %d", updatedTenant.CreditBalancePaise)
	}

	// Verify due status remains Paid (not modified)
	if due.Status != domain.DueStatusPaid {
		t.Fatalf("expected due status to remain paid, got %s", due.Status)
	}

	// Verify events: exactly one EvtOverpaymentCredited, zero EvtDuePaidOnTime/EvtDuePaidLate
	var foundOverpay bool
	for _, e := range pub.events {
		if e.EventType == domain.EvtDuePaidOnTime || e.EventType == domain.EvtDuePaidLate {
			t.Fatalf("must NOT emit due paid events on overpayment: got %s", e.EventType)
		}
		if e.EventType == domain.EvtPaymentMatched {
			t.Fatalf("must NOT emit EvtPaymentMatched on closed due: got %s", e.EventType)
		}
		if e.EventType == domain.EvtOverpaymentCredited {
			foundOverpay = true
		}
	}
	if !foundOverpay {
		t.Fatal("expected EvtOverpaymentCredited event to be published")
	}
}

func TestGatewaySettle_WebhookDedupKey_Idempotent(t *testing.T) {
	dueID := uuid.New()
	tenantID := uuid.New()
	propID := uuid.New()
	due := &domain.Due{
		ID: dueID, TenantID: tenantID, PropertyID: propID,
		DueCode: "DEDUP01", Amount: 5000, OriginalAmount: 5000,
		Status: domain.DueStatusPending, DueDate: time.Now().UTC(),
	}
	dues := &stubDues{byID: map[uuid.UUID]*domain.Due{dueID: due}}
	pays := &stubPayments{}
	tenants := &stubTenants{byID: map[uuid.UUID]*domain.Tenant{
		tenantID: {ID: tenantID, PropertyID: propID},
	}}
	pub := &recordingPublisher{}
	svc := NewService(dues, pays, tenants, nil, pub)

	dedup := "cashfree:pg:cf_order_pay_123"
	p1, err := svc.GatewaySettle(context.Background(), dueID, 5000, "cf_txn_123", dedup)
	if err != nil {
		t.Fatalf("first settle failed: %v", err)
	}
	if p1 == nil {
		t.Fatal("expected payment from first settle")
	}

	// Second settle with same dedupKey
	p2, err := svc.GatewaySettle(context.Background(), dueID, 5000, "cf_txn_123", dedup)
	if err != nil {
		t.Fatalf("duplicate settle failed: %v", err)
	}
	if p2 == nil {
		t.Fatal("expected payment on duplicate settle")
	}
	if p2.ID != p1.ID {
		t.Fatalf("expected same payment ID, got %v vs %v", p1.ID, p2.ID)
	}

	// Verify only 1 payment was created in storage
	if len(pays.created) != 1 {
		t.Fatalf("expected 1 payment in storage, got %d", len(pays.created))
	}
}
