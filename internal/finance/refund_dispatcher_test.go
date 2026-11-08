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
