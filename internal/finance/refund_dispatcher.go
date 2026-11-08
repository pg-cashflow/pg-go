package finance

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// RefundDispatcher defines the worker interface for dispatching refunds to Cashfree.
type RefundDispatcher interface {
	DispatchRefund(ctx context.Context, refundID uuid.UUID) error
}

// CashfreeRefundClient defines the Cashfree operations needed for refund dispatch.
type CashfreeRefundClient interface {
	CreateRefund(ctx context.Context, orderID, refundID string, amountPaise int64, reason, idempotencyKey string) (*cashfree.RefundDetails, error)
}

// RefundPaymentRepo abstracts the repository operations required for refund dispatch and reconciliation.
type RefundPaymentRepo interface {
	GetRefundByID(ctx context.Context, id uuid.UUID) (*domain.GatewayRefund, error)
	ReconcileRefundTransition(ctx context.Context, ref *domain.GatewayRefund, newStatus string) error
}

// RefundGatewayDispatcher handles gateway refund dispatch outbox events.
type RefundGatewayDispatcher struct {
	repo   RefundPaymentRepo
	client CashfreeRefundClient
}

// NewRefundGatewayDispatcher creates a new RefundGatewayDispatcher.
func NewRefundGatewayDispatcher(repo RefundPaymentRepo, client CashfreeRefundClient) *RefundGatewayDispatcher {
	return &RefundGatewayDispatcher{
		repo:   repo,
		client: client,
	}
}

// DispatchRefund processes an outbox-triggered refund against Cashfree.
func (d *RefundGatewayDispatcher) DispatchRefund(ctx context.Context, refundID uuid.UUID) error {
	if d.client == nil {
		return fmt.Errorf("refund dispatcher: cashfree client not configured")
	}
	if d.repo == nil {
		return fmt.Errorf("refund dispatcher: payment repo not configured")
	}

	ref, err := d.repo.GetRefundByID(ctx, refundID)
	if err != nil {
		return fmt.Errorf("refund dispatcher: get refund %s: %w", refundID, err)
	}
	if ref == nil {
		return fmt.Errorf("refund dispatcher: refund %s not found", refundID)
	}

	// Idempotency: If refund reached terminal status, safe no-op.
	status := domain.NormalizeRefundStatus(ref.Status)
	if status == domain.RefundStatusSucceeded || status == domain.RefundStatusFailed || status == domain.RefundStatusCancelled {
		return nil
	}

	// If already has CFRefundID, gateway already accepted the call. Poller or webhook will finalize.
	if ref.CFRefundID != nil && *ref.CFRefundID != "" {
		return nil
	}

	// Extract orderID from RawPayload
	var orderID string
	if len(ref.RawPayload) > 0 {
		var payloadMap map[string]any
		if err := json.Unmarshal(ref.RawPayload, &payloadMap); err == nil {
			if v, ok := payloadMap["order_id"].(string); ok {
				orderID = v
			}
		}
	}
	if orderID == "" {
		return fmt.Errorf("refund dispatcher: refund %s missing order_id in raw payload", refundID)
	}

	refundRef := ""
	if ref.RefundReference != nil {
		refundRef = *ref.RefundReference
	}
	if refundRef == "" {
		return fmt.Errorf("refund dispatcher: refund %s missing refund_reference", refundID)
	}

	cfIdempotencyKey := domain.CashfreeRefundIdempotencyKey(ref.ID)
	cfNote := ref.Reason
	if cfNote == "" {
		cfNote = fmt.Sprintf("Refund for payment %s", ref.PaymentID)
	}

	cfResp, cfErr := d.client.CreateRefund(ctx, orderID, refundRef, ref.AmountPaise, cfNote, cfIdempotencyKey)
	if cfErr != nil {
		slog.Error("refund dispatcher: cashfree create refund error",
			"error", cfErr, "payment_id", ref.PaymentID, "refund_id", ref.ID)

		disposition := cashfree.ClassifyRefundGatewayError(cfErr)
		if disposition == cashfree.RefundDefinitiveFailure {
			ref.Status = "failed"
			if reconErr := d.repo.ReconcileRefundTransition(ctx, ref, "failed"); reconErr != nil {
				return fmt.Errorf("refund dispatcher: cleanup failed refund %s: %w", ref.ID, reconErr)
			}
			return nil // definitive failure handled, do not retry outbox event
		}

		// Ambiguous failure: return error so outbox worker retries with backoff
		return fmt.Errorf("cashfree refund ambiguous error: %w", cfErr)
	}

	respStatus := domain.NormalizeRefundStatus(cfResp.RefundStatus)
	ref.Status = respStatus
	ref.CFRefundID = &cfResp.CFRefundID
	ref.ProviderRefundID = &cfResp.CFRefundID

	if err := d.repo.ReconcileRefundTransition(ctx, ref, respStatus); err != nil {
		return fmt.Errorf("refund dispatcher: reconcile refund %s status %s: %w", ref.ID, respStatus, err)
	}

	return nil
}
