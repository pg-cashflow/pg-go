package finance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
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
	locker postgres.RefundOperationLocker
}

// NewRefundGatewayDispatcher creates a new RefundGatewayDispatcher.
func NewRefundGatewayDispatcher(repo RefundPaymentRepo, client CashfreeRefundClient, locker ...postgres.RefundOperationLocker) *RefundGatewayDispatcher {
	var l postgres.RefundOperationLocker
	if len(locker) > 0 {
		l = locker[0]
	}
	return &RefundGatewayDispatcher{
		repo:   repo,
		client: client,
		locker: l,
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

	// R4: Mutual exclusion - acquire a 30s database lease (resource_leases) for refundID
	if d.locker != nil {
		acquired, unlock, err := d.locker.TryLockRefundOperation(ctx, refundID)
		if err != nil {
			return fmt.Errorf("refund dispatcher: acquire lock for refund %s: %w", refundID, err)
		}
		if !acquired {
			return fmt.Errorf("refund dispatcher: refund %s is busy with another operation", refundID)
		}
		defer unlock()
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
	if domain.IsTerminalRefundStatus(status) {
		return nil
	}

	// R3: Only 'initiated' refunds may execute CreateRefund.
	// 'pending', 'on_hold', and 'needs_review' are reconciliation-only.
	if status != domain.RefundStatusInitiated {
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

		// R6: Resolve ambiguous create by querying gateway first.
		resolved, recoverErr := d.recoverAmbiguousCreate(ctx, orderID, refundRef, ref, cfNote, cfErr)
		if recoverErr != nil {
			// Inconclusive status from gateway: transition to needs_review and stop automated creation retries
			ref.Status = "needs_review"
			if reconErr := d.repo.ReconcileRefundTransition(ctx, ref, "needs_review"); reconErr != nil {
				slog.Error("refund dispatcher: transition to needs_review failed", "error", reconErr, "refund_id", ref.ID)
				return fmt.Errorf("transition needs_review: %w", reconErr)
			}
			slog.Warn("refund dispatcher: ambiguous create transitioned to needs_review; automated retries stopped",
				"refund_id", ref.ID, "order_id", orderID, "error", recoverErr)
			return nil // Definitive outcome recorded; stop outbox worker retries
		}
		cfResp = resolved
	}

	// R1: Validate provider response strictly before trusting status or mutating local state
	if err := cashfree.ValidateRefundResponse(orderID, refundRef, ref.AmountPaise, cfResp); err != nil {
		slog.Error("refund dispatcher: gateway response validation failed; transitioning to needs_review",
			"error", err, "refund_id", ref.ID)
		if cfResp != nil && cfResp.CFRefundID != "" {
			ref.CFRefundID = &cfResp.CFRefundID
			ref.ProviderRefundID = &cfResp.CFRefundID
		}
		ref.Status = domain.RefundStatusNeedsReview
		if reconErr := d.repo.ReconcileRefundTransition(ctx, ref, domain.RefundStatusNeedsReview); reconErr != nil {
			slog.Error("refund dispatcher: transition to needs_review failed after validation failure", "error", reconErr, "refund_id", ref.ID)
			return fmt.Errorf("transition needs_review: %w", reconErr)
		}
		slog.Warn("refund dispatcher: validation failed transitioned to needs_review; automated retries stopped",
			"refund_id", ref.ID, "order_id", orderID, "error", err)
		return nil // Definitive outcome recorded; stop outbox worker retries
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

// RefundStatusFetcher is the optional read side of the Cashfree client used for recovery.
type RefundStatusFetcher interface {
	FetchRefundStatus(ctx context.Context, orderID, refundID string) (*cashfree.RefundDetails, error)
}

// isServerOrTransportError reports errors that mean "outcome unknown / not created",
// as opposed to rate limits, auth or idempotency conflicts, which never justify a new key.
func isServerOrTransportError(err error) bool {
	var apiErr *cashfree.APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= 500
	}
	return true
}

// recoverAmbiguousCreate resolves an ambiguous CreateRefund failure by consulting the gateway.
func (d *RefundGatewayDispatcher) recoverAmbiguousCreate(ctx context.Context, orderID, refundRef string, ref *domain.GatewayRefund, note string, cause error) (*cashfree.RefundDetails, error) {
	ambiguous := fmt.Errorf("cashfree refund ambiguous error: %w", cause)
	fetcher, ok := d.client.(RefundStatusFetcher)
	// A duplicate/conflict response means the first create probably succeeded (for example the
	// process crashed before it saved the result). A status lookup is read-only and safe, so it
	// runs here too. It never creates a refund and never rotates the key.
	if !ok || !(isServerOrTransportError(cause) || cashfree.IsRefundDuplicateError(cause)) {
		return nil, ambiguous
	}

	details, fetchErr := fetcher.FetchRefundStatus(ctx, orderID, refundRef)
	if fetchErr == nil && details != nil {
		if err := cashfree.ValidateRefundResponse(orderID, refundRef, ref.AmountPaise, details); err != nil {
			slog.Error("CRITICAL: refund validation failed on recovered gateway status; manual review required",
				"refund_id", ref.ID, "error", err)
			return nil, fmt.Errorf("refund dispatcher: recovered refund %s validation error: %w", ref.ID, err)
		}
		return details, nil
	}

	var apiErr *cashfree.APIError
	if !errors.As(fetchErr, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
		return nil, fmt.Errorf("cashfree refund status unconfirmed for refund %s: %w", ref.ID, cause)
	}

	// Gateway confirmed refund is absent (404) following an ambiguous 5xx/transport error.
	// Bounded retry: retry once with a refreshed idempotency key and identical refund_id.
	// Cashfree replays cached 5xx for a repeated idempotency key, so reusing the original key can never succeed.
	// The fixed refund_id protects against duplicate creation upstream.
	recoveryKey := domain.CashfreeRefundRecoveryIdempotencyKey(ref.ID, time.Now())
	fresh, createErr := d.client.CreateRefund(ctx, orderID, refundRef, ref.AmountPaise, note, recoveryKey)
	if createErr != nil {
		slog.Error("refund dispatcher: recovery create failed", "error", createErr, "refund_id", ref.ID)
		return nil, fmt.Errorf("cashfree refund recovery create error: %w", createErr)
	}
	return fresh, nil
}
