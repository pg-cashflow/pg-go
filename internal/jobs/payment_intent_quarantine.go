package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

const PaymentIntentQuarantineLockName = "payment_intent_quarantine"

// PaymentIntentQuarantineStore persists the recovery queue. Implementations
// must not mark a row resolved unless the worker has verified provider state.
type PaymentIntentQuarantineStore interface {
	ListReady(ctx context.Context, limit int) ([]domain.PaymentIntentQuarantine, error)
	RecordAttempt(ctx context.Context, id int64, status, providerStatus string, cause error, nextCheck time.Time) error
	MarkResolved(ctx context.Context, id int64, providerStatus string) error
}

type PaymentIntentQuarantineGateway interface {
	FetchSuccessfulPayment(ctx context.Context, orderID string) (cfPaymentID, bankRef string, amountPaise int64, ok bool, err error)
	GetOrder(ctx context.Context, orderID string) (*cashfree.OrderDetails, error)
	TerminateOrder(ctx context.Context, orderID string) (*cashfree.OrderDetails, error)
}

type PaymentIntentQuarantineSettler interface {
	SettleGatewayOrder(ctx context.Context, orderID, cfPaymentID, txnID string, amountPaise int64, intent *domain.PaymentIntent, rawPayload []byte) error
}

// PaymentIntentQuarantineJob actively closes old Cashfree orders and only
// releases their due reservations after terminal provider confirmation and a
// final successful-payment lookup.
type PaymentIntentQuarantineJob struct {
	Store   PaymentIntentQuarantineStore
	Gateway PaymentIntentQuarantineGateway
	Settler PaymentIntentQuarantineSettler
	Log     *slog.Logger
	Now     func() time.Time
}

func (j *PaymentIntentQuarantineJob) Run(ctx context.Context, limit int) error {
	if j == nil || j.Store == nil || j.Gateway == nil || j.Settler == nil {
		return errors.New("payment intent quarantine worker is not fully configured")
	}
	log := j.Log
	if log == nil {
		log = slog.Default()
	}
	now := time.Now().UTC()
	if j.Now != nil {
		now = j.Now().UTC()
	}
	items, err := j.Store.ListReady(ctx, limit)
	if err != nil {
		return err
	}
	var joined error
	for _, item := range items {
		if err := j.processOne(ctx, item, now); err != nil {
			log.Error("cashfree payment intent quarantine recovery failed",
				"quarantine_id", item.ID,
				"payment_intent_id", item.PaymentIntentID,
				"provider_order_id", item.ProviderOrderID,
				"err", err,
			)
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

func (j *PaymentIntentQuarantineJob) processOne(ctx context.Context, item domain.PaymentIntentQuarantine, now time.Time) error {
	if item.ID <= 0 || item.PaymentIntentID == (domain.PaymentIntent{}).ID || strings.TrimSpace(item.ProviderOrderID) == "" || item.AmountPaise <= 0 || len(item.DueIDs) == 0 {
		return j.retry(ctx, item, "needs_review", "", errors.New("quarantine record is missing required identifiers or amounts"), now)
	}

	// Reconcile a successful capture first. A capture does not release the due:
	// the provider order must also be terminal and every unmatched receipt must
	// have a completed owner action.
	paid, err := j.settleIfPaidResult(ctx, item)
	if err != nil {
		return j.retry(ctx, item, "needs_review", "", err, now)
	}

	order, err := j.Gateway.GetOrder(ctx, item.ProviderOrderID)
	if err != nil {
		status := "pending"
		if paid {
			status = "needs_review"
		}
		return j.retry(ctx, item, status, "", fmt.Errorf("read provider order: %w", err), now)
	}
	if order == nil || order.OrderID != item.ProviderOrderID {
		return j.retry(ctx, item, "needs_review", "", errors.New("provider order identity did not match quarantine record"), now)
	}
	if order.OrderAmountPaise != item.AmountPaise {
		return j.retry(ctx, item, "needs_review", order.OrderStatus,
			fmt.Errorf("provider order amount mismatch: got %d paise, expected %d", order.OrderAmountPaise, item.AmountPaise), now)
	}

	switch strings.ToUpper(strings.TrimSpace(order.OrderStatus)) {
	case "PAID":
		// The payments endpoint can lag the order state. Re-read it once before
		// placing the order in manual review.
		if !paid {
			paid, err = j.settleIfPaidResult(ctx, item)
			if err != nil {
				return j.retry(ctx, item, "needs_review", order.OrderStatus, err, now)
			}
		}
		if paid {
			return j.holdForPaidCapture(ctx, item, "PAID", now)
		}
		return j.retry(ctx, item, "needs_review", order.OrderStatus, errors.New("Cashfree order is PAID but no successful payment record is available yet"), now)
	case "EXPIRED", "TERMINATED":
		if paid {
			return j.holdForPaidCapture(ctx, item, order.OrderStatus, now)
		}
		return j.confirmTerminalOrderWithoutCapture(ctx, item, order.OrderStatus, now)
	case "TERMINATION_REQUESTED":
		if paid {
			return j.retry(ctx, item, "termination_requested", order.OrderStatus,
				errors.New("successful capture found while provider termination is still pending"), now)
		}
		return j.retry(ctx, item, "termination_requested", order.OrderStatus, nil, now)
	case "ACTIVE", "USER_DROPPED", "CREATED":
		terminated, termErr := j.Gateway.TerminateOrder(ctx, item.ProviderOrderID)
		if termErr != nil {
			status := "termination_requested"
			if paid {
				status = "needs_review"
			}
			providerStatus := order.OrderStatus
			if terminated != nil && strings.TrimSpace(terminated.OrderStatus) != "" {
				providerStatus = terminated.OrderStatus
			}
			return j.retry(ctx, item, status, providerStatus, fmt.Errorf("request provider termination: %w", termErr), now)
		}
		if terminated == nil {
			status := "termination_requested"
			if paid {
				status = "needs_review"
			}
			return j.retry(ctx, item, status, order.OrderStatus, errors.New("provider termination response did not include an order state"), now)
		}
		if terminated.OrderID != item.ProviderOrderID || terminated.OrderAmountPaise != item.AmountPaise {
			return j.retry(ctx, item, "needs_review", terminated.OrderStatus, errors.New("provider termination response identity or amount did not match quarantine record"), now)
		}
		switch strings.ToUpper(strings.TrimSpace(terminated.OrderStatus)) {
		case "PAID":
			if !paid {
				paid, err = j.settleIfPaidResult(ctx, item)
				if err != nil {
					return j.retry(ctx, item, "needs_review", "PAID", err, now)
				}
			}
			if !paid {
				return j.retry(ctx, item, "needs_review", "PAID", errors.New("provider reports PAID but payment record is not available"), now)
			}
			return j.holdForPaidCapture(ctx, item, "PAID", now)
		case "EXPIRED", "TERMINATED":
			if paid {
				return j.holdForPaidCapture(ctx, item, terminated.OrderStatus, now)
			}
			return j.confirmTerminalOrderWithoutCapture(ctx, item, terminated.OrderStatus, now)
		case "ACTIVE", "TERMINATION_REQUESTED":
			status := "termination_requested"
			cause := error(nil)
			if paid {
				status = "needs_review"
				cause = errors.New("late capture found while Cashfree order termination remains pending")
				if err := j.Store.RecordAttempt(ctx, item.ID, status, terminated.OrderStatus, cause, nextQuarantineCheck(item, now)); err != nil {
					return fmt.Errorf("persist quarantine retry: %w", err)
				}
				return nil
			}
			return j.retry(ctx, item, status, terminated.OrderStatus, cause, now)
		default:
			return j.retry(ctx, item, "needs_review", terminated.OrderStatus, errors.New("provider returned an unknown order state after termination request"), now)
		}
	default:
		return j.retry(ctx, item, "needs_review", order.OrderStatus, fmt.Errorf("unsupported Cashfree order state %q", order.OrderStatus), now)
	}
}

// confirmTerminalOrderWithoutCapture performs a payment lookup after the
// provider order read. The earlier pre-read lookup cannot be the release check:
// a capture may become visible while the order transitions to EXPIRED or
// TERMINATED. Every terminal observation therefore needs its own subsequent
// payment lookup, and two matching terminal observations are still required.
func (j *PaymentIntentQuarantineJob) confirmTerminalOrderWithoutCapture(
	ctx context.Context,
	item domain.PaymentIntentQuarantine,
	providerStatus string,
	now time.Time,
) error {
	paid, err := j.settleIfPaidResult(ctx, item)
	if err != nil {
		return j.retry(ctx, item, "needs_review", providerStatus,
			fmt.Errorf("final successful-payment lookup after terminal order read: %w", err), now)
	}
	if paid {
		return j.holdForPaidCapture(ctx, item, providerStatus, now)
	}
	if !terminalStateConfirmedAgain(item, providerStatus) {
		return j.retry(ctx, item, "needs_review", providerStatus, nil, now)
	}
	return j.resolveTerminalOrder(ctx, item, providerStatus, now)
}

func (j *PaymentIntentQuarantineJob) resolveTerminalOrder(ctx context.Context, item domain.PaymentIntentQuarantine, providerStatus string, now time.Time) error {
	if err := j.Store.MarkResolved(ctx, item.ID, providerStatus); err != nil {
		// A terminal provider order is not sufficient when an earlier successful
		// capture still has an unresolved owner receipt. Persist backoff instead
		// of retrying the provider every scheduler tick.
		return j.retry(ctx, item, "needs_review", providerStatus, err, now)
	}
	return nil
}

// A successful late capture is recorded by the canonical settlement path as an
// unapplied receipt. It is not safe to release the due at this point: the owner
// must assign the money as tenant credit or complete a refund first.
func (j *PaymentIntentQuarantineJob) holdForPaidCapture(ctx context.Context, item domain.PaymentIntentQuarantine, providerStatus string, now time.Time) error {
	cause := errors.New("late Cashfree capture recorded as an unapplied receipt; due remains reserved until the provider order is terminal and the owner resolves the receipt")
	return j.Store.RecordAttempt(ctx, item.ID, "needs_review", providerStatus, cause, nextQuarantineCheck(item, now))
}

func terminalStateConfirmedAgain(item domain.PaymentIntentQuarantine, providerStatus string) bool {
	if item.Status != "needs_review" || item.LastProviderStatus == nil {
		return false
	}
	previous := strings.ToUpper(strings.TrimSpace(*item.LastProviderStatus))
	current := strings.ToUpper(strings.TrimSpace(providerStatus))
	return current != "" && previous == current && (current == "EXPIRED" || current == "TERMINATED")
}

func (j *PaymentIntentQuarantineJob) settleIfPaidResult(ctx context.Context, item domain.PaymentIntentQuarantine) (bool, error) {
	cfID, bankRef, amount, ok, err := j.Gateway.FetchSuccessfulPayment(ctx, item.ProviderOrderID)
	if err != nil {
		return false, fmt.Errorf("fetch successful provider payment: %w", err)
	}
	if !ok {
		return false, nil
	}
	if amount != item.AmountPaise {
		return false, fmt.Errorf("captured amount mismatch: got %d paise, expected %d", amount, item.AmountPaise)
	}
	txnID := strings.TrimSpace(bankRef)
	if txnID == "" {
		txnID = cfID
	}
	intent := item.Intent
	if intent.ID == (domain.PaymentIntent{}).ID {
		intent = domain.PaymentIntent{
			ID:              item.PaymentIntentID,
			DueID:           item.DueID,
			Provider:        "cashfree",
			ProviderOrderID: item.ProviderOrderID,
			AmountPaise:     item.AmountPaise,
			Status:          domain.IntentSuperseded,
		}
	}
	if err := j.Settler.SettleGatewayOrder(ctx, item.ProviderOrderID, cfID, txnID, amount, &intent, nil); err != nil {
		return false, fmt.Errorf("settle quarantined Cashfree payment: %w", err)
	}
	return true, nil
}

// nextQuarantineCheck applies bounded exponential backoff. It is shared by
// retry failures and expected needs-review holds so the provider is not polled
// on every scheduler tick.
func nextQuarantineCheck(item domain.PaymentIntentQuarantine, now time.Time) time.Time {
	attempt := item.AttemptCount
	if attempt < 0 {
		attempt = 0
	}
	delay := 10 * time.Second
	for i := 0; i < attempt && delay < 30*time.Minute; i++ {
		delay *= 2
	}
	if delay > 30*time.Minute {
		delay = 30 * time.Minute
	}
	return now.Add(delay)
}

func (j *PaymentIntentQuarantineJob) retry(
	ctx context.Context,
	item domain.PaymentIntentQuarantine,
	status string,
	providerStatus string,
	cause error,
	now time.Time,
) error {
	if err := j.Store.RecordAttempt(ctx, item.ID, status, providerStatus, cause, nextQuarantineCheck(item, now)); err != nil {
		return errors.Join(cause, fmt.Errorf("persist quarantine retry: %w", err))
	}
	return cause
}
