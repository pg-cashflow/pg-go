package finance

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type mockRefundRepo struct {
	refunds      map[uuid.UUID]*domain.GatewayRefund
	reconcileLog []string
}

func (m *mockRefundRepo) GetRefundByID(ctx context.Context, id uuid.UUID) (*domain.GatewayRefund, error) {
	r, ok := m.refunds[id]
	if !ok {
		return nil, domain.ErrNotFound
	}
	return r, nil
}

func (m *mockRefundRepo) ReconcileRefundTransition(ctx context.Context, ref *domain.GatewayRefund, newStatus string) error {
	ref.Status = newStatus
	m.reconcileLog = append(m.reconcileLog, newStatus)
	return nil
}

type mockCFRefundClient struct {
	calls      int
	resp       *cashfree.RefundDetails
	err        error
	lastReason string
	lastIdem   string
}

func (m *mockCFRefundClient) CreateRefund(ctx context.Context, orderID, refundID string, amountPaise int64, reason, idempotencyKey string) (*cashfree.RefundDetails, error) {
	m.calls++
	m.lastReason = reason
	m.lastIdem = idempotencyKey
	if m.err != nil {
		return nil, m.err
	}
	return m.resp, nil
}

func TestRefundGatewayDispatcher_TerminalNoOp(t *testing.T) {
	refID := uuid.New()
	repo := &mockRefundRepo{
		refunds: map[uuid.UUID]*domain.GatewayRefund{
			refID: {
				ID:     refID,
				Status: "succeeded",
			},
		},
	}
	cf := &mockCFRefundClient{}
	d := NewRefundGatewayDispatcher(repo, cf)

	err := d.DispatchRefund(context.Background(), refID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cf.calls != 0 {
		t.Errorf("expected 0 gateway calls for terminal refund, got %d", cf.calls)
	}
}

func TestRefundGatewayDispatcher_DefinitiveFailureMarksFailed(t *testing.T) {
	refID := uuid.New()
	refRef := "rf_test_123"
	raw, _ := json.Marshal(map[string]any{"order_id": "order_123"})
	repo := &mockRefundRepo{
		refunds: map[uuid.UUID]*domain.GatewayRefund{
			refID: {
				ID:              refID,
				RefundReference: &refRef,
				Status:          "initiated",
				AmountPaise:     50000,
				RawPayload:      raw,
			},
		},
	}
	cf := &mockCFRefundClient{
		err: &cashfree.APIError{StatusCode: http.StatusBadRequest, Status: "400 Bad Request"},
	}
	d := NewRefundGatewayDispatcher(repo, cf)

	err := d.DispatchRefund(context.Background(), refID)
	if err != nil {
		t.Fatalf("expected nil error on definitive failure, got %v", err)
	}
	if cf.calls != 1 {
		t.Fatalf("expected 1 gateway call, got %d", cf.calls)
	}
	if len(repo.reconcileLog) != 1 || repo.reconcileLog[0] != "failed" {
		t.Errorf("expected reconcile to 'failed', got %v", repo.reconcileLog)
	}
}

func TestRefundGatewayDispatcher_AmbiguousFailureReturnsError(t *testing.T) {
	refID := uuid.New()
	refRef := "rf_test_123"
	raw, _ := json.Marshal(map[string]any{"order_id": "order_123"})
	repo := &mockRefundRepo{
		refunds: map[uuid.UUID]*domain.GatewayRefund{
			refID: {
				ID:              refID,
				RefundReference: &refRef,
				Status:          "initiated",
				AmountPaise:     50000,
				RawPayload:      raw,
			},
		},
	}
	cf := &mockCFRefundClient{
		err: &cashfree.APIError{StatusCode: http.StatusUnprocessableEntity, Status: "422 Unprocessable"},
	}
	d := NewRefundGatewayDispatcher(repo, cf)

	err := d.DispatchRefund(context.Background(), refID)
	if err == nil {
		t.Fatal("expected error on ambiguous 422 failure to trigger outbox retry, got nil")
	}
	if len(repo.reconcileLog) != 0 {
		t.Errorf("expected no reconcile on ambiguous error, got %v", repo.reconcileLog)
	}
}

func TestRefundGatewayDispatcher_SuccessReconcilesStatus(t *testing.T) {
	refID := uuid.New()
	refRef := "rf_test_123"
	raw, _ := json.Marshal(map[string]any{"order_id": "order_123"})
	repo := &mockRefundRepo{
		refunds: map[uuid.UUID]*domain.GatewayRefund{
			refID: {
				ID:              refID,
				RefundReference: &refRef,
				Status:          "initiated",
				AmountPaise:     50000,
				RawPayload:      raw,
			},
		},
	}
	cf := &mockCFRefundClient{
		resp: &cashfree.RefundDetails{
			CFRefundID:   "cf_ref_xyz",
			RefundStatus: "SUCCESS",
			AmountPaise:  50000,
		},
	}
	d := NewRefundGatewayDispatcher(repo, cf)

	err := d.DispatchRefund(context.Background(), refID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(repo.reconcileLog) != 1 || repo.reconcileLog[0] != "succeeded" {
		t.Errorf("expected reconcile to 'succeeded', got %v", repo.reconcileLog)
	}
	saved := repo.refunds[refID]
	if saved.CFRefundID == nil || *saved.CFRefundID != "cf_ref_xyz" {
		t.Errorf("expected CFRefundID 'cf_ref_xyz', got %v", saved.CFRefundID)
	}
}

// fetchingCFRefundClient extends the mock with FetchRefundStatus and per-call
// scripted CreateRefund results, so the replayed-5xx recovery path can be exercised.
type fetchingCFRefundClient struct {
	createResults []error
	createResp    *cashfree.RefundDetails
	createKeys    []string
	fetchResp     *cashfree.RefundDetails
	fetchErr      error
	fetchCalls    int
}

func (m *fetchingCFRefundClient) CreateRefund(ctx context.Context, orderID, refundID string, amountPaise int64, reason, idempotencyKey string) (*cashfree.RefundDetails, error) {
	i := len(m.createKeys)
	m.createKeys = append(m.createKeys, idempotencyKey)
	if i < len(m.createResults) && m.createResults[i] != nil {
		return nil, m.createResults[i]
	}
	return m.createResp, nil
}

func (m *fetchingCFRefundClient) FetchRefundStatus(ctx context.Context, orderID, refundID string) (*cashfree.RefundDetails, error) {
	m.fetchCalls++
	return m.fetchResp, m.fetchErr
}

func newInitiatedRefundRepo(refID uuid.UUID) *mockRefundRepo {
	refRef := "rf_test_123"
	raw, _ := json.Marshal(map[string]any{"order_id": "order_123"})
	return &mockRefundRepo{refunds: map[uuid.UUID]*domain.GatewayRefund{
		refID: {ID: refID, RefundReference: &refRef, Status: "initiated", AmountPaise: 50000, RawPayload: raw},
	}}
}

// A 5xx on the first attempt may be cached and replayed by Cashfree for the same
// idempotency key. When the refund does not exist at the gateway (404), the retry
// must use a NEW key while keeping the same refund_id.
func TestRefundGatewayDispatcher_Replayed5xxRetriesWithFreshKey(t *testing.T) {
	refID := uuid.New()
	repo := newInitiatedRefundRepo(refID)
	cf := &fetchingCFRefundClient{
		createResults: []error{&cashfree.APIError{StatusCode: http.StatusBadGateway, Status: "502"}, nil},
		createResp:    &cashfree.RefundDetails{CFRefundID: "cf_1", RefundStatus: "PENDING", AmountPaise: 50000},
		fetchErr:      &cashfree.APIError{StatusCode: http.StatusNotFound, Status: "404"},
	}
	d := NewRefundGatewayDispatcher(repo, cf)

	if err := d.DispatchRefund(context.Background(), refID); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cf.createKeys) != 2 {
		t.Fatalf("expected 2 create calls, got %d", len(cf.createKeys))
	}
	if cf.createKeys[0] != domain.CashfreeRefundIdempotencyKey(refID) {
		t.Errorf("first attempt must use deterministic key")
	}
	if cf.createKeys[1] == cf.createKeys[0] {
		t.Errorf("retry after 5xx must rotate the idempotency key")
	}
	if len(repo.reconcileLog) != 1 || repo.reconcileLog[0] != "pending" {
		t.Errorf("expected reconcile to 'pending', got %v", repo.reconcileLog)
	}
}

// If the 5xx happened AFTER Cashfree created the refund, we must adopt the
// gateway's record and never create again.
func TestRefundGatewayDispatcher_5xxButRefundExistsAdoptsGatewayState(t *testing.T) {
	refID := uuid.New()
	repo := newInitiatedRefundRepo(refID)
	cf := &fetchingCFRefundClient{
		createResults: []error{&cashfree.APIError{StatusCode: http.StatusInternalServerError, Status: "500"}},
		fetchResp:     &cashfree.RefundDetails{CFRefundID: "cf_9", RefundStatus: "SUCCESS", AmountPaise: 50000},
	}
	d := NewRefundGatewayDispatcher(repo, cf)

	if err := d.DispatchRefund(context.Background(), refID); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cf.createKeys) != 1 {
		t.Fatalf("must not re-create when refund exists at gateway; create calls=%d", len(cf.createKeys))
	}
	saved := repo.refunds[refID]
	if saved.CFRefundID == nil || *saved.CFRefundID != "cf_9" || saved.Status != "succeeded" {
		t.Errorf("expected adopted cf_9/succeeded, got %v/%s", saved.CFRefundID, saved.Status)
	}
}

// Amount drift between local row and gateway must never auto-reconcile.
func TestRefundGatewayDispatcher_5xxGatewayAmountMismatchReturnsError(t *testing.T) {
	refID := uuid.New()
	repo := newInitiatedRefundRepo(refID)
	cf := &fetchingCFRefundClient{
		createResults: []error{&cashfree.APIError{StatusCode: http.StatusInternalServerError, Status: "500"}},
		fetchResp:     &cashfree.RefundDetails{CFRefundID: "cf_9", RefundStatus: "SUCCESS", AmountPaise: 99900},
	}
	d := NewRefundGatewayDispatcher(repo, cf)

	if err := d.DispatchRefund(context.Background(), refID); err == nil {
		t.Fatal("expected error on gateway amount mismatch")
	}
	if len(repo.reconcileLog) != 0 {
		t.Errorf("must not reconcile on mismatch, got %v", repo.reconcileLog)
	}
}

// 429/401/403 are not "refund missing" signals; do not rotate keys on them.
func TestRefundGatewayDispatcher_RateLimitDoesNotRotateKey(t *testing.T) {
	refID := uuid.New()
	repo := newInitiatedRefundRepo(refID)
	cf := &fetchingCFRefundClient{
		createResults: []error{&cashfree.APIError{StatusCode: http.StatusTooManyRequests, Status: "429"}},
		fetchErr:      &cashfree.APIError{StatusCode: http.StatusNotFound, Status: "404"},
	}
	d := NewRefundGatewayDispatcher(repo, cf)

	if err := d.DispatchRefund(context.Background(), refID); err == nil {
		t.Fatal("expected error so outbox retries with backoff")
	}
	if len(cf.createKeys) != 1 {
		t.Errorf("429 must not trigger an inline re-create; calls=%d", len(cf.createKeys))
	}
}
