package jobs

import (
	"context"
	"fmt"
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
	settle := &stubSettle{}
	job := &CashfreePollJob{
		Intents: intents,
		Dues:    stubDueByID{dueID: &domain.Due{ID: dueID, Status: domain.DueStatusPending}},
		Client:  stubCFFetch{ok: true, cfID: "cf_pay_998877", ref: "UTR_BANK_RECON_88", amount: 750000},
		Settle:  settle,
		Now:     func() time.Time { return time.Now().UTC() },
	}

	if err := job.Run(context.Background()); err != nil {
		t.Fatalf("job.Run failed: %v", err)
	}

	if settle.n != 1 {
		t.Fatalf("expected exactly 1 settlement invocation, got %d", settle.n)
	}
	if settle.dueID != dueID {
		t.Fatalf("expected settled dueID=%s, got %s", dueID, settle.dueID)
	}
	if settle.amount != 750000 {
		t.Fatalf("expected settled amount=750000, got %d", settle.amount)
	}
	if settle.txn != "UTR_BANK_RECON_88" {
		t.Fatalf("expected txn='UTR_BANK_RECON_88', got '%s'", settle.txn)
	}
	if intents.cfPaid != "cf_pay_998877" {
		t.Fatalf("expected intent marked paid with cf_pay_998877, got '%s'", intents.cfPaid)
	}
	if intents.touched != 1 {
		t.Fatalf("expected intent touched count=1, got %d", intents.touched)
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

func (s stubRefundFetch) FetchRefundStatus(context.Context, string, string) (*cashfree.RefundDetails, error) {
	if s.err != nil {
		return nil, s.err
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

func TestCashfreePollResubmitsInitiatedRefundOn404(t *testing.T) {
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

	if resubmitClient.createCalls != 1 {
		t.Fatalf("expected exactly 1 CreateRefund resubmission call, got %d", resubmitClient.createCalls)
	}
	if resubmitClient.lastIdempKey == domain.CashfreeRefundIdempotencyKey(refID) {
		t.Fatalf("recovery retry must not reuse initial deterministic idempotency key %s", resubmitClient.lastIdempKey)
	}
	if _, err := uuid.Parse(resubmitClient.lastIdempKey); err != nil {
		t.Fatalf("idempotency key is not a valid UUID: %v", err)
	}
	if resubmitClient.lastRefundID != refRef {
		t.Fatalf("expected constant refund_id %q, got %q", refRef, resubmitClient.lastRefundID)
	}
	expectedReason := fmt.Sprintf("Refund %s", refRef)
	if resubmitClient.lastReason != expectedReason {
		t.Fatalf("expected refund note %q, got %q", expectedReason, resubmitClient.lastReason)
	}
	if refundRepo.reconciledID != refID || refundRepo.reconciledStatus != "succeeded" {
		t.Fatalf("expected resubmitted refund to reconcile to succeeded, got id=%s status=%s",
			refundRepo.reconciledID, refundRepo.reconciledStatus)
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

func TestCashfreePollResubmitDuplicateDoesNotMarkFailed(t *testing.T) {
	refRef := "rf_dup_exists_1"
	refID := uuid.New()
	cfRefID := "cf_rf_dup_exists_1"
	staleCreatedAt := time.Now().UTC().Add(-5 * time.Minute)
	staleRef := domain.GatewayRefund{
		ID:              refID,
		RefundReference: &refRef,
		RawPayload:      []byte(`{"order_id": "order_dup_1"}`),
		Status:          "initiated",
		AmountPaise:     150000,
		Reason:          "Deposit refund duplicate test",
		CreatedAt:       staleCreatedAt,
	}
	refundRepo := &stubRefundReconciler{
		stubRefundStale: stubRefundStale{list: []domain.GatewayRefund{staleRef}},
	}
	resubmitClient := &stubRefundResubmitClient{
		fetchErr: &cashfree.APIError{StatusCode: http.StatusNotFound, Status: "404 Not Found", Body: "not found"},
		createErr: &cashfree.APIError{
			StatusCode: http.StatusBadRequest,
			Status:     "400 Bad Request",
			Body:       `{"message":"refund_id already exists","code":"refund_already_exists"}`,
		},
		fetchDetails: &cashfree.RefundDetails{
			CFRefundID:   cfRefID,
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

	if refundRepo.reconciledID != refID || refundRepo.reconciledStatus != "succeeded" {
		t.Fatalf("expected duplicate refund to re-query status and reconcile to succeeded, got id=%s status=%s",
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

var _ GatewaySettler = (*payment.Service)(nil)
