package jobs

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
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
	amount int64
}

func (s stubCFFetch) FetchSuccessfulPayment(context.Context, string) (string, string, int64, bool, error) {
	return s.cfID, s.ref, s.amount, s.ok, nil
}

type stubSettle struct {
	n      int
	dueID  uuid.UUID
	amount int64
	txn    string
	err    error
}

func (s *stubSettle) GatewaySettle(_ context.Context, dueID uuid.UUID, amountPaise int64, txnID string, _ ...string) (*domain.Payment, error) {
	s.n++
	s.dueID = dueID
	s.amount = amountPaise
	s.txn = txnID
	if s.err != nil {
		return nil, s.err
	}
	return &domain.Payment{DueID: dueID, Amount: amountPaise, MatchedBy: domain.MatchedByCashfree}, nil
}

type pollStubOrderSettler struct {
	n       int
	orderID string
	cfID    string
	bankRef string
	amount  int64
	err     error
	intents *stubIntentStale
}

func (s *pollStubOrderSettler) SettleGatewayOrder(
	ctx context.Context,
	orderID string,
	cfPaymentID string,
	bankRef string,
	amountPaise int64,
	intent *domain.PaymentIntent,
	_ []byte,
) error {
	s.n++
	s.orderID = orderID
	s.cfID = cfPaymentID
	s.bankRef = bankRef
	s.amount = amountPaise
	if s.err != nil {
		return s.err
	}
	if s.intents != nil && intent != nil && cfPaymentID != "" {
		_ = s.intents.MarkPaid(ctx, intent.ID, cfPaymentID)
	}
	return nil
}

func TestCashfreePollSettlesOpenDue(t *testing.T) {
	dueID := uuid.New()
	intentID := uuid.New()
	intents := &stubIntentStale{list: []domain.PaymentIntent{{
		ID: intentID, DueID: dueID, ProviderOrderID: "pg-X-1", Status: domain.IntentCreated, AmountPaise: 5000,
	}}}
	settler := &pollStubOrderSettler{intents: intents}
	job := &CashfreePollJob{
		Intents:      intents,
		Dues:         stubDueByID{dueID: &domain.Due{ID: dueID, Status: domain.DueStatusPending}},
		Client:       stubCFFetch{ok: true, cfID: "cf1", ref: "UTR1", amount: 5000},
		OrderSettler: settler,
		Now:          func() time.Time { return time.Now().UTC() },
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if settler.n != 1 || settler.bankRef != "UTR1" || settler.cfID != "cf1" || intents.cfPaid != "cf1" {
		t.Fatalf("settler=%+v paid=%s", settler, intents.cfPaid)
	}
	if intents.touched != 1 {
		t.Fatalf("touch=%d", intents.touched)
	}
}

func TestCashfreePollJob_SettlesAfterWebhookRejected(t *testing.T) {
	// Scenario: An inbound Cashfree webhook arrived with an anomaly (e.g. malformed currency or empty status),
	// causing the webhook handler to reject the payload and refuse immediate settlement.
	// The payment intent remains in 'created' status in the database.
	// When the reconciliation poll job runs, it detects the stale intent, contacts Cashfree directly,
	// confirms successful payment upstream, and settles the pending due.
	dueID := uuid.New()
	intentID := uuid.New()
	orderID := "pg-order-webhook-rejected-101"

	intents := &stubIntentStale{list: []domain.PaymentIntent{{
		ID:              intentID,
		DueID:           dueID,
		ProviderOrderID: orderID,
		Status:          domain.IntentCreated,
		AmountPaise:     750000,
	}}}
	settler := &pollStubOrderSettler{intents: intents}
	job := &CashfreePollJob{
		Intents:      intents,
		Dues:         stubDueByID{dueID: &domain.Due{ID: dueID, Status: domain.DueStatusPending}},
		Client:       stubCFFetch{ok: true, cfID: "cf_pay_998877", ref: "UTR_BANK_RECON_88", amount: 750000},
		OrderSettler: settler,
		Now:          func() time.Time { return time.Now().UTC() },
	}

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("job.Run failed: %v", err)
	}

	if settler.n != 1 {
		t.Fatalf("expected exactly 1 settlement invocation, got %d", settler.n)
	}
	if settler.orderID != orderID {
		t.Fatalf("expected settled orderID=%s, got %s", orderID, settler.orderID)
	}
	if settler.amount != 750000 {
		t.Fatalf("expected settled amount=750000, got %d", settler.amount)
	}
	if settler.bankRef != "UTR_BANK_RECON_88" {
		t.Fatalf("expected bankRef='UTR_BANK_RECON_88', got '%s'", settler.bankRef)
	}
	if intents.cfPaid != "cf_pay_998877" {
		t.Fatalf("expected intent marked paid with cf_pay_998877, got '%s'", intents.cfPaid)
	}
	if intents.touched != 1 {
		t.Fatalf("expected intent touched count=1, got %d", intents.touched)
	}
}

func TestCashfreePollRoutesMatchingCaptureForPaidDue(t *testing.T) {
	dueID := uuid.New()
	intents := &stubIntentStale{list: []domain.PaymentIntent{{
		ID: uuid.New(), DueID: dueID, ProviderOrderID: "pg-X-1", AmountPaise: 5000,
	}}}
	settler := &pollStubOrderSettler{intents: intents}
	job := &CashfreePollJob{
		Intents:      intents,
		Dues:         stubDueByID{dueID: &domain.Due{ID: dueID, Status: domain.DueStatusPaid}},
		Client:       stubCFFetch{ok: true, cfID: "cf1", amount: 5000},
		OrderSettler: settler,
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if settler.n != 1 {
		t.Fatalf("expected OrderSettler to handle paid due capture, n=%d", settler.n)
	}
	if settler.orderID != "pg-X-1" || settler.cfID != "cf1" || settler.amount != 5000 {
		t.Fatalf("unexpected settler args: %+v", settler)
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
	settler := &pollStubOrderSettler{err: payment.ErrDueNotOpen, intents: intents}
	job := &CashfreePollJob{
		Intents:      intents,
		Dues:         stubDueByID{dueID: &domain.Due{ID: dueID, Status: domain.DueStatusPending}},
		Client:       stubCFFetch{ok: true, cfID: "cf1", ref: "UTR1", amount: 5000},
		OrderSettler: settler,
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if intents.cfPaid != "" {
		t.Fatalf("must not MarkPaid when settlement errors, paid=%s", intents.cfPaid)
	}
	if intents.touched != 1 {
		t.Fatalf("touch=%d", intents.touched)
	}
}

func TestCashfreePollFailsClosedWithoutOrderSettler(t *testing.T) {
	dueID := uuid.New()
	intents := &stubIntentStale{list: []domain.PaymentIntent{{
		ID: uuid.New(), DueID: dueID, ProviderOrderID: "pg-X-1", AmountPaise: 5000,
	}}}
	settle := &stubSettle{}
	job := &CashfreePollJob{
		Intents: intents,
		Dues:    stubDueByID{dueID: &domain.Due{ID: dueID, Status: domain.DueStatusPending}},
		Client:  stubCFFetch{ok: true, cfID: "cf1", ref: "UTR1", amount: 5000},
		Settle:  settle,
	}
	if err := job.Run(context.Background()); err == nil {
		t.Fatal("expected error when OrderSettler is nil")
	}
	if settle.n != 0 || intents.cfPaid != "" {
		t.Fatalf("legacy Settle/MarkPaid must not run when OrderSettler is nil, settle=%d paid=%s", settle.n, intents.cfPaid)
	}
}

func TestCashfreePoll_DoesNotFallbackBankRefToCFPaymentID(t *testing.T) {
	dueID := uuid.New()
	intents := &stubIntentStale{list: []domain.PaymentIntent{{
		ID: uuid.New(), DueID: dueID, ProviderOrderID: "pg-X-2", AmountPaise: 5000,
	}}}
	settler := &pollStubOrderSettler{intents: intents}
	job := &CashfreePollJob{
		Intents:      intents,
		Dues:         stubDueByID{dueID: &domain.Due{ID: dueID, Status: domain.DueStatusPending}},
		Client:       stubCFFetch{ok: true, cfID: "cf_only_99", ref: "", amount: 5000},
		OrderSettler: settler,
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if settler.bankRef != "" {
		t.Fatalf("expected empty bankRef when bank_reference absent, got %q", settler.bankRef)
	}
	if settler.cfID != "cf_only_99" {
		t.Fatalf("expected cfID cf_only_99, got %q", settler.cfID)
	}
}

type stubRefundStale struct {
	list    []domain.GatewayRefund
	updated *domain.GatewayRefund
}

func (s *stubRefundStale) ListStaleNonTerminalRefunds(context.Context, time.Time) ([]domain.GatewayRefund, error) {
	return s.list, nil
}

func (s *stubRefundStale) CreateOrUpdateRefund(_ context.Context, ref *domain.GatewayRefund) error {
	s.updated = ref
	return nil
}

type stubRefundFetch struct {
	details *cashfree.RefundDetails
	err     error
}

func (s stubRefundFetch) FetchRefundStatus(_ context.Context, orderID, refundID string) (*cashfree.RefundDetails, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.details != nil && s.details.OrderID == "" {
		s.details.OrderID = orderID
	}
	return s.details, nil
}

func TestCashfreePollReconcilesStuckRefund(t *testing.T) {
	cfRefID := "cf_rf_99"
	refRef := "rf_dup_1"
	staleRef := domain.GatewayRefund{
		ID:              uuid.New(),
		CFRefundID:      &cfRefID,
		RefundReference: &refRef,
		RawPayload:      []byte(`{"order_id": "order_stuck_1"}`),
		Status:          "pending", // stuck!
		AmountPaise:     550000,
	}
	refundRepo := &stubRefundStale{list: []domain.GatewayRefund{staleRef}}
	refundClient := stubRefundFetch{
		details: &cashfree.RefundDetails{
			CFRefundID:   cfRefID,
			RefundID:     refRef,
			RefundStatus: "SUCCESS",
			AmountPaise:  550000,
		},
	}

	job := &CashfreePollJob{
		Refunds:      refundRepo,
		RefundClient: refundClient,
		Now:          func() time.Time { return time.Now().UTC() },
	}

	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if refundRepo.updated == nil || refundRepo.updated.Status != "succeeded" {
		t.Fatalf("expected stuck refund to transition to succeeded: %+v", refundRepo.updated)
	}
}

func TestCashfreePollReconcilesOnHoldRefund(t *testing.T) {
	cfRefID := "cf_rf_hold_1"
	refRef := "rf_dup_hold"
	staleRef := domain.GatewayRefund{
		ID:              uuid.New(),
		CFRefundID:      &cfRefID,
		RefundReference: &refRef,
		RawPayload:      []byte(`{"order_id": "order_hold_1"}`),
		Status:          "initiated",
		AmountPaise:     550000,
	}
	refundRepo := &stubRefundStale{list: []domain.GatewayRefund{staleRef}}
	refundClient := stubRefundFetch{
		details: &cashfree.RefundDetails{
			CFRefundID:   cfRefID,
			RefundID:     refRef,
			RefundStatus: "ONHOLD",
			AmountPaise:  550000,
		},
	}

	job := &CashfreePollJob{
		Refunds:      refundRepo,
		RefundClient: refundClient,
		Now:          func() time.Time { return time.Now().UTC() },
	}

	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if refundRepo.updated == nil || refundRepo.updated.Status != domain.RefundStatusOnHold {
		t.Fatalf("expected ONHOLD refund to transition to %s, got: %+v", domain.RefundStatusOnHold, refundRepo.updated)
	}
}

func TestCashfreePollSkipsAmountMismatch(t *testing.T) {
	dueID := uuid.New()
	intents := &stubIntentStale{list: []domain.PaymentIntent{{
		ID: uuid.New(), DueID: dueID, ProviderOrderID: "pg-X-1", AmountPaise: 5000,
	}}}
	settler := &pollStubOrderSettler{intents: intents}
	job := &CashfreePollJob{
		Intents:      intents,
		Dues:         stubDueByID{dueID: &domain.Due{ID: dueID, Status: domain.DueStatusPending}},
		Client:       stubCFFetch{ok: true, cfID: "cf1", amount: 9999},
		OrderSettler: settler,
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if settler.n != 0 || intents.cfPaid != "" {
		t.Fatalf("mismatch must not settle n=%d paid=%s", settler.n, intents.cfPaid)
	}
}

type stubRefundReconciler struct {
	stubRefundStale
	reconciledID     uuid.UUID
	reconciledStatus string
}

func (s *stubRefundReconciler) ReconcileRefundTransition(_ context.Context, ref *domain.GatewayRefund, newStatus string) error {
	s.reconciledID = ref.ID
	s.reconciledStatus = newStatus
	ref.Status = newStatus
	s.updated = ref
	return nil
}

func (s *stubRefundReconciler) GetRefundDueIDs(_ context.Context, refundID uuid.UUID) ([]uuid.UUID, error) {
	return []uuid.UUID{uuid.MustParse("00000000-0000-0000-0000-000000000010")}, nil
}

type stubReferralClawback struct {
	reversedDueID     uuid.UUID
	reversedPaymentID uuid.UUID
}

func (s *stubReferralClawback) ReverseFirstRentPayment(_ context.Context, dueID, paymentID uuid.UUID) error {
	s.reversedDueID = dueID
	s.reversedPaymentID = paymentID
	return nil
}

func TestCashfreePollReconcilesStuckRefundWithReconciler(t *testing.T) {
	cfRefID := "cf_rf_rec_1"
	refRef := "rf_rec_1"
	refID := uuid.New()
	staleRef := domain.GatewayRefund{
		ID:              refID,
		CFRefundID:      &cfRefID,
		RefundReference: &refRef,
		RawPayload:      []byte(`{"order_id": "order_rec_1"}`),
		Status:          "pending",
		AmountPaise:     550000,
	}
	refundRepo := &stubRefundReconciler{
		stubRefundStale: stubRefundStale{list: []domain.GatewayRefund{staleRef}},
	}
	refundClient := stubRefundFetch{
		details: &cashfree.RefundDetails{
			CFRefundID:   cfRefID,
			RefundID:     refRef,
			RefundStatus: "SUCCESS",
			AmountPaise:  550000,
		},
	}

	job := &CashfreePollJob{
		Refunds:      refundRepo,
		RefundClient: refundClient,
		Now:          func() time.Time { return time.Now().UTC() },
	}

	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if refundRepo.reconciledID != refID || refundRepo.reconciledStatus != "succeeded" {
		t.Fatalf("expected ReconcileRefundTransition to be called with succeeded for %s, got id=%s status=%s", refID, refundRepo.reconciledID, refundRepo.reconciledStatus)
	}
}

func TestCashfreePollReconcilesStuckRefundClawsBackReferral(t *testing.T) {
	cfRefID := "cf_rf_rec_2"
	refRef := "rf_rec_2"
	refID := uuid.New()
	paymentID := uuid.New()
	expectedDueID := uuid.MustParse("00000000-0000-0000-0000-000000000010")
	staleRef := domain.GatewayRefund{
		ID:              refID,
		PaymentID:       paymentID,
		CFRefundID:      &cfRefID,
		RefundReference: &refRef,
		RawPayload:      []byte(`{"order_id": "order_rec_2"}`),
		Status:          "pending",
		AmountPaise:     550000,
	}
	refundRepo := &stubRefundReconciler{
		stubRefundStale: stubRefundStale{list: []domain.GatewayRefund{staleRef}},
	}
	refundClient := stubRefundFetch{
		details: &cashfree.RefundDetails{
			CFRefundID:   cfRefID,
			RefundID:     refRef,
			RefundStatus: "SUCCESS",
			AmountPaise:  550000,
		},
	}
	referralTracker := &stubReferralClawback{}

	job := &CashfreePollJob{
		Refunds:      refundRepo,
		RefundClient: refundClient,
		Referrals:    referralTracker,
		Now:          func() time.Time { return time.Now().UTC() },
	}

	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if referralTracker.reversedDueID != expectedDueID || referralTracker.reversedPaymentID != paymentID {
		t.Fatalf("expected referral reversal for due %s payment %s, got due=%s payment=%s",
			expectedDueID, paymentID, referralTracker.reversedDueID, referralTracker.reversedPaymentID)
	}
}

func TestCashfreePollReconcilesStuckInitiatedRefundToOnHoldOn404(t *testing.T) {
	refRef := "rf_orphan_1"
	refID := uuid.New()
	staleCreatedAt := time.Now().UTC().Add(-45 * time.Minute)
	staleRef := domain.GatewayRefund{
		ID:              refID,
		RefundReference: &refRef,
		RawPayload:      []byte(`{"order_id": "order_orphan_1"}`),
		Status:          "initiated",
		AmountPaise:     200000,
		CreatedAt:       staleCreatedAt,
	}
	refundRepo := &stubRefundReconciler{
		stubRefundStale: stubRefundStale{list: []domain.GatewayRefund{staleRef}},
	}
	refundClient := stubRefundFetch{
		err: &cashfree.APIError{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: "refund not found"},
	}

	job := &CashfreePollJob{
		Refunds:      refundRepo,
		RefundClient: refundClient,
		Now:          func() time.Time { return time.Now().UTC() },
	}

	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if refundRepo.reconciledID != refID || refundRepo.reconciledStatus != domain.RefundStatusNeedsReview {
		t.Fatalf("expected orphan initiated refund to reconcile to needs_review, got id=%s status=%s",
			refundRepo.reconciledID, refundRepo.reconciledStatus)
	}
}

func TestCashfreePollIgnores500With404InBody(t *testing.T) {
	refRef := "rf_server_err_1"
	refID := uuid.New()
	staleCreatedAt := time.Now().UTC().Add(-45 * time.Minute)
	staleRef := domain.GatewayRefund{
		ID:              refID,
		RefundReference: &refRef,
		RawPayload:      []byte(`{"order_id": "order_err_1"}`),
		Status:          "initiated",
		AmountPaise:     200000,
		CreatedAt:       staleCreatedAt,
	}
	refundRepo := &stubRefundReconciler{
		stubRefundStale: stubRefundStale{list: []domain.GatewayRefund{staleRef}},
	}
	// Gateway returns 500 error containing "404" in error body text
	refundClient := stubRefundFetch{
		err: &cashfree.APIError{
			StatusCode: http.StatusInternalServerError,
			Status:     "500 Internal Server Error",
			Body:       "upstream database error: entity 404 query failed",
		},
	}

	job := &CashfreePollJob{
		Refunds:      refundRepo,
		RefundClient: refundClient,
		Now:          func() time.Time { return time.Now().UTC() },
	}

	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if refundRepo.reconciledID != uuid.Nil || refundRepo.reconciledStatus != "" {
		t.Fatalf("500 with 404 in body must not reconcile refund to failed, got id=%s status=%s",
			refundRepo.reconciledID, refundRepo.reconciledStatus)
	}
}

func TestCashfreePollLeavesInitiatedRefundWhenOrderIDEmpty(t *testing.T) {
	refRef := "rf_no_order_1"
	refID := uuid.New()
	staleCreatedAt := time.Now().UTC().Add(-45 * time.Minute)
	staleRef := domain.GatewayRefund{
		ID:              refID,
		RefundReference: &refRef,
		Status:          "initiated",
		AmountPaise:     200000,
		CreatedAt:       staleCreatedAt,
	}
	refundRepo := &stubRefundReconciler{
		stubRefundStale: stubRefundStale{list: []domain.GatewayRefund{staleRef}},
	}
	refundClient := stubRefundFetch{
		err: &cashfree.APIError{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: "not found"},
	}

	job := &CashfreePollJob{
		Refunds:      refundRepo,
		RefundClient: refundClient,
		Now:          func() time.Time { return time.Now().UTC() },
	}

	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if refundRepo.reconciledID != uuid.Nil || refundRepo.reconciledStatus != "" {
		t.Fatalf("refund with empty orderID must not be marked failed, got id=%s status=%s",
			refundRepo.reconciledID, refundRepo.reconciledStatus)
	}
}

type stubRefundResubmitClient struct {
	fetchDetails *cashfree.RefundDetails
	fetchErr     error
	createCalls  int
	lastOrderID  string
	lastRefundID string
	lastAmount   int64
	lastReason   string
	lastIdempKey string
	createdResp  *cashfree.RefundDetails
	createErr    error
}

func (s *stubRefundResubmitClient) FetchRefundStatus(context.Context, string, string) (*cashfree.RefundDetails, error) {
	if s.createCalls > 0 && s.fetchDetails != nil {
		return s.fetchDetails, nil
	}
	return s.fetchDetails, s.fetchErr
}

func (s *stubRefundResubmitClient) CreateRefund(_ context.Context, orderID, refundID string, amountPaise int64, reason, idempotencyKey string) (*cashfree.RefundDetails, error) {
	s.createCalls++
	s.lastOrderID = orderID
	s.lastRefundID = refundID
	s.lastAmount = amountPaise
	s.lastReason = reason
	s.lastIdempKey = idempotencyKey
	return s.createdResp, s.createErr
}

func TestCashfreePollInitiated404WithinGraceWindowDoesNotReconcile(t *testing.T) {
	refRef := "rf_resubmit_1"
	refID := uuid.New()
	cfRefID := "cf_rf_resubmitted_999"
	staleCreatedAt := time.Now().UTC().Add(-5 * time.Minute)
	staleRef := domain.GatewayRefund{
		ID:              refID,
		RefundReference: &refRef,
		RawPayload:      []byte(`{"order_id": "order_resubmit_1"}`),
		Status:          "initiated",
		AmountPaise:     150000,
		Reason:          "Tenant early deposit return",
		CreatedAt:       staleCreatedAt,
	}
	refundRepo := &stubRefundReconciler{
		stubRefundStale: stubRefundStale{list: []domain.GatewayRefund{staleRef}},
	}
	resubmitClient := &stubRefundResubmitClient{
		fetchErr: &cashfree.APIError{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: "refund not found"},
		createdResp: &cashfree.RefundDetails{
			CFRefundID:   cfRefID,
			OrderID:      "order_resubmit_1",
			RefundID:     refRef,
			RefundStatus: "SUCCESS",
			AmountPaise:  150000,
		},
	}

	job := &CashfreePollJob{
		Refunds:      refundRepo,
		RefundClient: resubmitClient,
		Now:          func() time.Time { return time.Now().UTC() },
	}

	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if resubmitClient.createCalls != 0 {
		t.Fatalf("expected 0 CreateRefund calls from poller, got %d", resubmitClient.createCalls)
	}
	if refundRepo.reconciledID != uuid.Nil || refundRepo.reconciledStatus != "" {
		t.Fatalf("expected refund within grace window to remain unchanged, got id=%s status=%s",
			refundRepo.reconciledID, refundRepo.reconciledStatus)
	}
}

type stubRefundAlerter struct {
	calls []struct {
		refundID    string
		orderID     string
		amountPaise int64
		reason      string
		age         time.Duration
	}
}

func (s *stubRefundAlerter) NotifyRefundStuck(ctx context.Context, refundID, orderID string, amountPaise int64, reason string, age time.Duration) error {
	s.calls = append(s.calls, struct {
		refundID    string
		orderID     string
		amountPaise int64
		reason      string
		age         time.Duration
	}{refundID: refundID, orderID: orderID, amountPaise: amountPaise, reason: reason, age: age})
	return nil
}

func TestCashfreePollInitiated404BeyondGracePeriodReconcilesToNeedsReview(t *testing.T) {
	refRef := "rf_resubmit_2"
	refID := uuid.New()
	staleCreatedAt := time.Now().UTC().Add(-20 * time.Minute)
	staleRef := domain.GatewayRefund{
		ID:              refID,
		RefundReference: &refRef,
		RawPayload:      []byte(`{"order_id": "order_resubmit_2"}`),
		Status:          "initiated",
		AmountPaise:     150000,
		Reason:          "Tenant early deposit return",
		CreatedAt:       staleCreatedAt,
	}
	refundRepo := &stubRefundReconciler{
		stubRefundStale: stubRefundStale{list: []domain.GatewayRefund{staleRef}},
	}
	resubmitClient := &stubRefundResubmitClient{
		fetchErr: &cashfree.APIError{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: "refund not found"},
	}

	alerter := &stubRefundAlerter{}
	job := &CashfreePollJob{
		Refunds:       refundRepo,
		RefundClient:  resubmitClient,
		RefundAlerter: alerter,
		Now:           func() time.Time { return time.Now().UTC() },
	}

	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if resubmitClient.createCalls != 0 {
		t.Fatalf("expected 0 CreateRefund calls from poller, got %d", resubmitClient.createCalls)
	}
	if refundRepo.reconciledID != refID || refundRepo.reconciledStatus != domain.RefundStatusNeedsReview {
		t.Fatalf("expected refund beyond 15m grace window to reconcile to needs_review, got id=%s status=%s",
			refundRepo.reconciledID, refundRepo.reconciledStatus)
	}
	if len(alerter.calls) != 1 || alerter.calls[0].refundID != refRef {
		t.Fatalf("expected 1 stuck alert for %s, got %d calls", refRef, len(alerter.calls))
	}
}

func TestCashfreePollReconcilesRejectedRefundToFailed(t *testing.T) {
	refRef := "rf_rejected_1"
	refID := uuid.New()
	cfRefID := "cf_rf_rejected_1"
	staleCreatedAt := time.Now().UTC().Add(-5 * time.Minute)
	staleRef := domain.GatewayRefund{
		ID:              refID,
		RefundReference: &refRef,
		RawPayload:      []byte(`{"order_id": "order_rej_1"}`),
		Status:          "pending",
		AmountPaise:     200000,
		Reason:          "Deposit return",
		CreatedAt:       staleCreatedAt,
	}
	refundRepo := &stubRefundReconciler{
		stubRefundStale: stubRefundStale{list: []domain.GatewayRefund{staleRef}},
	}
	client := &stubRefundFetch{
		details: &cashfree.RefundDetails{
			CFRefundID:   cfRefID,
			RefundID:     refRef,
			RefundStatus: "REJECTED",
			AmountPaise:  200000,
		},
	}

	job := &CashfreePollJob{
		Refunds:      refundRepo,
		RefundClient: client,
		Now:          func() time.Time { return time.Now().UTC() },
	}

	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	if refundRepo.reconciledID != refID || refundRepo.reconciledStatus != "failed" {
		t.Fatalf("expected rejected refund to reconcile to failed, got id=%s status=%s",
			refundRepo.reconciledID, refundRepo.reconciledStatus)
	}
}

type stubCounting429Client struct {
	calls int
}

func (s *stubCounting429Client) FetchRefundStatus(context.Context, string, string) (*cashfree.RefundDetails, error) {
	s.calls++
	return nil, &cashfree.APIError{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests"}
}

func TestCashfreePollStopsOn429RateLimit(t *testing.T) {
	refRef1 := "rf_429_1"
	ref1 := domain.GatewayRefund{
		ID:              uuid.New(),
		RefundReference: &refRef1,
		RawPayload:      []byte(`{"order_id": "order_429_1"}`),
		Status:          "pending",
		AmountPaise:     10000,
	}
	refRef2 := "rf_429_2"
	ref2 := domain.GatewayRefund{
		ID:              uuid.New(),
		RefundReference: &refRef2,
		RawPayload:      []byte(`{"order_id": "order_429_2"}`),
		Status:          "pending",
		AmountPaise:     20000,
	}

	countingClient := &stubCounting429Client{}
	refundRepo := &stubRefundReconciler{
		stubRefundStale: stubRefundStale{list: []domain.GatewayRefund{ref1, ref2}},
	}

	job := &CashfreePollJob{
		Refunds:      refundRepo,
		RefundClient: countingClient,
		Now:          func() time.Time { return time.Now().UTC() },
	}

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("expected nil on 429 rate limit backoff, got: %v", err)
	}

	if countingClient.calls != 1 {
		t.Fatalf("expected exactly 1 call before rate limit abort, got: %d", countingClient.calls)
	}
}

func TestCashfreePoll_RejectsOrderIDMismatch(t *testing.T) {
	refRef := "rf_test_oid_mismatch"
	ref := domain.GatewayRefund{
		ID:              uuid.New(),
		RefundReference: &refRef,
		RawPayload:      []byte(`{"order_id": "order_expected_123"}`),
		Status:          "initiated",
		AmountPaise:     50000,
	}
	refundRepo := &stubRefundReconciler{
		stubRefundStale: stubRefundStale{list: []domain.GatewayRefund{ref}},
	}
	refundClient := stubRefundFetch{
		details: &cashfree.RefundDetails{
			OrderID:      "order_different_999",
			RefundID:     refRef,
			CFRefundID:   "cf_rf_mismatch_1",
			RefundStatus: "SUCCESS",
			AmountPaise:  50000,
		},
	}
	job := &CashfreePollJob{
		Refunds:      refundRepo,
		RefundClient: refundClient,
		Now:          func() time.Time { return time.Now().UTC() },
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if refundRepo.reconciledStatus != "" || refundRepo.updated != nil {
		t.Fatalf("expected no refund mutation on order_id mismatch, got status=%q updated=%+v", refundRepo.reconciledStatus, refundRepo.updated)
	}
}

func TestCashfreePoll_RejectsRefundIDMismatch(t *testing.T) {
	refRef := "rf_test_ref_expected"
	ref := domain.GatewayRefund{
		ID:              uuid.New(),
		RefundReference: &refRef,
		RawPayload:      []byte(`{"order_id": "order_123"}`),
		Status:          "initiated",
		AmountPaise:     50000,
	}
	refundRepo := &stubRefundReconciler{
		stubRefundStale: stubRefundStale{list: []domain.GatewayRefund{ref}},
	}
	refundClient := stubRefundFetch{
		details: &cashfree.RefundDetails{
			OrderID:      "order_123",
			RefundID:     "rf_different_456",
			CFRefundID:   "cf_rf_mismatch_2",
			RefundStatus: "SUCCESS",
			AmountPaise:  50000,
		},
	}
	job := &CashfreePollJob{
		Refunds:      refundRepo,
		RefundClient: refundClient,
		Now:          func() time.Time { return time.Now().UTC() },
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if refundRepo.reconciledStatus != "" || refundRepo.updated != nil {
		t.Fatalf("expected no refund mutation on refund_id mismatch, got status=%q updated=%+v", refundRepo.reconciledStatus, refundRepo.updated)
	}
}

func TestCashfreePoll_RejectsAmountMismatch(t *testing.T) {
	refRef := "rf_test_amt_mismatch"
	ref := domain.GatewayRefund{
		ID:              uuid.New(),
		RefundReference: &refRef,
		RawPayload:      []byte(`{"order_id": "order_123"}`),
		Status:          "initiated",
		AmountPaise:     50000,
	}
	refundRepo := &stubRefundReconciler{
		stubRefundStale: stubRefundStale{list: []domain.GatewayRefund{ref}},
	}
	refundClient := stubRefundFetch{
		details: &cashfree.RefundDetails{
			OrderID:      "order_123",
			RefundID:     refRef,
			CFRefundID:   "cf_rf_mismatch_3",
			RefundStatus: "SUCCESS",
			AmountPaise:  40000, // mismatch!
		},
	}
	job := &CashfreePollJob{
		Refunds:      refundRepo,
		RefundClient: refundClient,
		Now:          func() time.Time { return time.Now().UTC() },
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if refundRepo.reconciledStatus != "" || refundRepo.updated != nil {
		t.Fatalf("expected no refund mutation on amount mismatch, got status=%q updated=%+v", refundRepo.reconciledStatus, refundRepo.updated)
	}
}

func TestCashfreePoll_RejectsCFRefundIDMismatch(t *testing.T) {
	refRef := "rf_test_cfid_mismatch"
	storedCFID := "cf_existing_111"
	ref := domain.GatewayRefund{
		ID:              uuid.New(),
		RefundReference: &refRef,
		CFRefundID:      &storedCFID,
		RawPayload:      []byte(`{"order_id": "order_123"}`),
		Status:          "initiated",
		AmountPaise:     50000,
	}
	refundRepo := &stubRefundReconciler{
		stubRefundStale: stubRefundStale{list: []domain.GatewayRefund{ref}},
	}
	refundClient := stubRefundFetch{
		details: &cashfree.RefundDetails{
			OrderID:      "order_123",
			RefundID:     refRef,
			CFRefundID:   "cf_different_222", // mismatch against stored!
			RefundStatus: "SUCCESS",
			AmountPaise:  50000,
		},
	}
	job := &CashfreePollJob{
		Refunds:      refundRepo,
		RefundClient: refundClient,
		Now:          func() time.Time { return time.Now().UTC() },
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if refundRepo.reconciledStatus != "" || refundRepo.updated != nil {
		t.Fatalf("expected no refund mutation on cf_refund_id mismatch, got status=%q updated=%+v", refundRepo.reconciledStatus, refundRepo.updated)
	}
}

func TestCashfreePoll_PersistsDiscoveredCFRefundIDWhenStatusUnchanged(t *testing.T) {
	refRef := "rf_test_discover_cfid"
	ref := domain.GatewayRefund{
		ID:              uuid.New(),
		RefundReference: &refRef,
		CFRefundID:      nil, // not yet populated locally
		RawPayload:      []byte(`{"order_id": "order_123"}`),
		Status:          "pending",
		AmountPaise:     50000,
	}
	refundRepo := &stubRefundStale{list: []domain.GatewayRefund{ref}}
	refundClient := stubRefundFetch{
		details: &cashfree.RefundDetails{
			OrderID:      "order_123",
			RefundID:     refRef,
			CFRefundID:   "cf_discovered_777",
			RefundStatus: "PENDING", // status is unchanged ("pending" == "pending")
			AmountPaise:  50000,
		},
	}
	job := &CashfreePollJob{
		Refunds:      refundRepo,
		RefundClient: refundClient,
		Now:          func() time.Time { return time.Now().UTC() },
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if refundRepo.updated == nil {
		t.Fatalf("expected refund to be persisted with newly discovered cf_refund_id")
	}
	if refundRepo.updated.CFRefundID == nil || *refundRepo.updated.CFRefundID != "cf_discovered_777" {
		t.Fatalf("expected cf_refund_id to be cf_discovered_777, got: %v", refundRepo.updated.CFRefundID)
	}
}

func TestCashfreePollStuckBeyond72HoursAlertsOperator(t *testing.T) {
	refRef := "rf_stuck_72h"
	refID := uuid.New()
	staleCreatedAt := time.Now().UTC().Add(-75 * time.Hour)
	staleRef := domain.GatewayRefund{
		ID:              refID,
		RefundReference: &refRef,
		RawPayload:      []byte(`{"order_id": "order_stuck_72h"}`),
		Status:          "pending",
		AmountPaise:     200000,
		Reason:          "Deposit return",
		CreatedAt:       staleCreatedAt,
	}
	refundRepo := &stubRefundStale{list: []domain.GatewayRefund{staleRef}}
	refundClient := stubRefundFetch{
		details: &cashfree.RefundDetails{
			OrderID:      "order_stuck_72h",
			RefundID:     refRef,
			CFRefundID:   "cf_stuck_72h",
			RefundStatus: "PENDING",
			AmountPaise:  200000,
		},
	}
	alerter := &stubRefundAlerter{}
	job := &CashfreePollJob{
		Refunds:       refundRepo,
		RefundClient:  refundClient,
		RefundAlerter: alerter,
		Now:           func() time.Time { return time.Now().UTC() },
	}
	if err := job.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(alerter.calls) != 1 || alerter.calls[0].refundID != refRef {
		t.Fatalf("expected 1 critical alert for stuck refund, got %d", len(alerter.calls))
	}
}

var _ GatewaySettler = (*payment.Service)(nil)
