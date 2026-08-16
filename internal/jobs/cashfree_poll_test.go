package jobs

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

type stubIntentStale struct {
	list    []domain.PaymentIntent
	paid    uuid.UUID
	cfPaid  string
	touched int
}

func (s *stubIntentStale) ListStaleCreated(context.Context, time.Time) ([]domain.PaymentIntent, error) {
	return s.list, nil
}
func (s *stubIntentStale) MarkPaid(_ context.Context, id uuid.UUID, cfPaymentID string) error {
	s.paid = id
	s.cfPaid = cfPaymentID
	return nil
}
func (s *stubIntentStale) Touch(context.Context, uuid.UUID) error {
	s.touched++
	return nil
}

type stubDueByID map[uuid.UUID]*domain.Due

func (s stubDueByID) GetByID(_ context.Context, id uuid.UUID) (*domain.Due, error) {
	return s[id], nil
}

type stubCFFetch struct {
	ok     bool
	cfID   string
	ref    string
	amount int
}

func (s stubCFFetch) FetchSuccessfulPayment(context.Context, string) (string, string, int, bool, error) {
	return s.cfID, s.ref, s.amount, s.ok, nil
}

type stubSettle struct {
	n      int
	dueID  uuid.UUID
	amount int
	txn    string
	err    error
}

func (s *stubSettle) GatewaySettle(_ context.Context, dueID uuid.UUID, amountPaise int, txnID string) (*domain.Payment, error) {
	s.n++
	s.dueID = dueID
	s.amount = amountPaise
	s.txn = txnID
	if s.err != nil {
		return nil, s.err
	}
	return &domain.Payment{DueID: dueID, Amount: amountPaise, MatchedBy: domain.MatchedByCashfree}, nil
}

func TestCashfreePollSettlesOpenDue(t *testing.T) {
	dueID := uuid.New()
	intentID := uuid.New()
	intents := &stubIntentStale{list: []domain.PaymentIntent{{
		ID: intentID, DueID: dueID, ProviderOrderID: "pg-X-1", Status: domain.IntentCreated, AmountPaise: 5000,
	}}}
	settle := &stubSettle{}
	job := &CashfreePollJob{
		Intents: intents,
		Dues:    stubDueByID{dueID: &domain.Due{ID: dueID, Status: domain.DueStatusPending}},
		Client:  stubCFFetch{ok: true, cfID: "cf1", ref: "UTR1", amount: 5000},
		Settle:  settle,
		Now:     func() time.Time { return time.Now().UTC() },
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if settle.n != 1 || settle.txn != "UTR1" || intents.cfPaid != "cf1" {
		t.Fatalf("settle=%+v paid=%s", settle, intents.cfPaid)
	}
	if intents.touched != 1 {
		t.Fatalf("touch=%d", intents.touched)
	}
}

func TestCashfreePollSkipsPaidDue(t *testing.T) {
	dueID := uuid.New()
	intents := &stubIntentStale{list: []domain.PaymentIntent{{
		ID: uuid.New(), DueID: dueID, ProviderOrderID: "pg-X-1",
	}}}
	settle := &stubSettle{}
	job := &CashfreePollJob{
		Intents: intents,
		Dues:    stubDueByID{dueID: &domain.Due{ID: dueID, Status: domain.DueStatusPaid}},
		Client:  stubCFFetch{ok: true, cfID: "cf1", amount: 1},
		Settle:  settle,
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if settle.n != 0 {
		t.Fatalf("paid due must not settle, n=%d", settle.n)
	}
}

func TestCashfreePollNoopWithoutClient(t *testing.T) {
	job := &CashfreePollJob{Intents: &stubIntentStale{list: []domain.PaymentIntent{{ID: uuid.New()}}}}
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestCashfreePollSkipsMarkPaidOnDueNotOpen(t *testing.T) {
	dueID := uuid.New()
	intents := &stubIntentStale{list: []domain.PaymentIntent{{
		ID: uuid.New(), DueID: dueID, ProviderOrderID: "pg-X-1", AmountPaise: 5000,
	}}}
	settle := &stubSettle{err: payment.ErrDueNotOpen}
	job := &CashfreePollJob{
		Intents: intents,
		Dues:    stubDueByID{dueID: &domain.Due{ID: dueID, Status: domain.DueStatusPending}},
		Client:  stubCFFetch{ok: true, cfID: "cf1", ref: "UTR1", amount: 5000},
		Settle:  settle,
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if intents.cfPaid != "" {
		t.Fatalf("must not MarkPaid when due is not open, paid=%s", intents.cfPaid)
	}
	if intents.touched != 1 {
		t.Fatalf("touch=%d", intents.touched)
	}
}

func TestCashfreePollSkipsAmountMismatch(t *testing.T) {
	dueID := uuid.New()
	intents := &stubIntentStale{list: []domain.PaymentIntent{{
		ID: uuid.New(), DueID: dueID, ProviderOrderID: "pg-X-1", AmountPaise: 5000,
	}}}
	settle := &stubSettle{}
	job := &CashfreePollJob{
		Intents: intents,
		Dues:    stubDueByID{dueID: &domain.Due{ID: dueID, Status: domain.DueStatusPending}},
		Client:  stubCFFetch{ok: true, cfID: "cf1", amount: 9999},
		Settle:  settle,
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if settle.n != 0 || intents.cfPaid != "" {
		t.Fatalf("mismatch must not settle n=%d paid=%s", settle.n, intents.cfPaid)
	}
}

var _ GatewaySettler = (*payment.Service)(nil)
