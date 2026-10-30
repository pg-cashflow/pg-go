package jobs

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
	"github.com/pg-cashflow/pg-go/internal/payment"
)

const (
	CashfreePollStale       = 10 * time.Minute
	CashfreeRefundPollStale = 1 * time.Hour
)

type IntentStaleLister interface {
	ListStaleCreated(ctx context.Context, olderThan time.Time) ([]domain.PaymentIntent, error)
	MarkPaid(ctx context.Context, id uuid.UUID, cfPaymentID string) error
	Touch(ctx context.Context, id uuid.UUID) error
}

type DueByID interface {
	GetByID(ctx context.Context, id uuid.UUID) (*domain.Due, error)
}

type CashfreeFetcher interface {
	FetchSuccessfulPayment(ctx context.Context, orderID string) (cfPaymentID, bankRef string, amountPaise int64, ok bool, err error)
}

type GatewaySettler interface {
	GatewaySettle(ctx context.Context, dueID uuid.UUID, amountPaise int64, txnID string, dedupKey ...string) (*domain.Payment, error)
}

type RefundStaleRepo interface {
	ListStaleNonTerminalRefunds(ctx context.Context, olderThan time.Time) ([]domain.GatewayRefund, error)
	CreateOrUpdateRefund(ctx context.Context, ref *domain.GatewayRefund) error
}

type RefundReconciler interface {
	ReconcileRefundTransition(ctx context.Context, ref *domain.GatewayRefund, newStatus string) error
}

type RefundDueLister interface {
	GetRefundDueIDs(ctx context.Context, refundID uuid.UUID) ([]uuid.UUID, error)
}

type ReferralClawback interface {
	ReverseFirstRentPayment(ctx context.Context, dueID, paymentID uuid.UUID) error
}

type PaymentOrderResolver interface {
	GetPaymentOrderID(ctx context.Context, paymentID uuid.UUID) (string, error)
}

type RefundStatusFetcher interface {
	FetchRefundStatus(ctx context.Context, orderID, refundID string) (*cashfree.RefundDetails, error)
}

// CashfreePollJob settles created intents whose webhook was missed, and reconciles stuck refunds.
type CashfreePollJob struct {
	Intents          IntentStaleLister
	Dues             DueByID
	Client           CashfreeFetcher
	Settle           GatewaySettler
	Refunds          RefundStaleRepo
	RefundClient     RefundStatusFetcher
	Referrals        ReferralClawback
	StaleAfter       time.Duration
	RefundStaleAfter time.Duration
	Now              func() time.Time
	Log              *slog.Logger
}

func (j *CashfreePollJob) Run(ctx context.Context) error {
	if j.Client != nil && j.Intents != nil {
		if err := j.runPaymentIntents(ctx); err != nil {
			return err
		}
	}
	if j.Refunds != nil && j.RefundClient != nil {
		if err := j.runRefunds(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (j *CashfreePollJob) runPaymentIntents(ctx context.Context) error {
	log := j.Log
	if log == nil {
		log = slog.Default()
	}
	staleAfter := j.StaleAfter
	if staleAfter <= 0 {
		staleAfter = CashfreePollStale
	}
	now := time.Now().UTC()
	if j.Now != nil {
		now = j.Now()
	}
	stale, err := j.Intents.ListStaleCreated(ctx, now.Add(-staleAfter))
	if err != nil {
		return err
	}
	for _, intent := range stale {
		if j.Dues != nil {
			due, err := j.Dues.GetByID(ctx, intent.DueID)
			if err != nil || due == nil {
				continue
			}
			if due.Status != domain.DueStatusPending && due.Status != domain.DueStatusPartial {
				continue
			}
		}
		cfID, bankRef, amount, ok, err := j.Client.FetchSuccessfulPayment(ctx, intent.ProviderOrderID)
		_ = j.Intents.Touch(ctx, intent.ID)
		if err != nil {
			log.Error("cashfree poll fetch", "order", intent.ProviderOrderID, "err", err)
			continue
		}
		if !ok {
			continue
		}
		if amount != intent.AmountPaise {
			log.Error("cashfree poll amount mismatch", "order", intent.ProviderOrderID, "got", amount, "want", intent.AmountPaise)
			continue
		}
		txn := bankRef
		if txn == "" {
			txn = cfID
		}
		dedupKey := ""
		if cfID != "" {
			dedupKey = fmt.Sprintf("cashfree:pg:%s", cfID)
		}
		if dedupKey != "" {
			_, err = j.Settle.GatewaySettle(ctx, intent.DueID, amount, txn, dedupKey)
		} else {
			_, err = j.Settle.GatewaySettle(ctx, intent.DueID, amount, txn)
		}
		if err != nil && !errors.Is(err, payment.ErrDuplicateTxn) {
			log.Error("cashfree poll settle", "order", intent.ProviderOrderID, "err", err)
			continue
		}
		if cfID != "" {
			_ = j.Intents.MarkPaid(ctx, intent.ID, cfID)
		}
	}
	return nil
}

func (j *CashfreePollJob) runRefunds(ctx context.Context) error {
	log := j.Log
	if log == nil {
		log = slog.Default()
	}
	staleAfter := j.RefundStaleAfter
	if staleAfter <= 0 {
		staleAfter = CashfreeRefundPollStale
	}
	now := time.Now().UTC()
	if j.Now != nil {
		now = j.Now()
	}
	staleRefunds, err := j.Refunds.ListStaleNonTerminalRefunds(ctx, now.Add(-staleAfter))
	if err != nil {
		return err
	}
	for _, ref := range staleRefunds {
		refundID := ""
		if ref.RefundReference != nil && *ref.RefundReference != "" {
			refundID = *ref.RefundReference
		} else if ref.CFRefundID != nil {
			refundID = *ref.CFRefundID
		}
		if refundID == "" {
			continue
		}
		orderID := ""
		hasVerifiedOrderID := false
		if len(ref.RawPayload) > 0 {
			var rawMap map[string]any
			if err := json.Unmarshal(ref.RawPayload, &rawMap); err == nil {
				if oid, ok := rawMap["order_id"].(string); ok && oid != "" {
					orderID = oid
					hasVerifiedOrderID = true
				} else if data, ok := rawMap["data"].(map[string]any); ok {
					if rdata, ok := data["refund"].(map[string]any); ok {
						if oid, ok := rdata["order_id"].(string); ok && oid != "" {
							orderID = oid
							hasVerifiedOrderID = true
						}
					}
				}
			}
		}
		if orderID == "" {
			if resolver, ok := j.Refunds.(PaymentOrderResolver); ok {
				if oid, err := resolver.GetPaymentOrderID(ctx, ref.PaymentID); err == nil && oid != "" {
					orderID = oid
				}
			}
		}
		if orderID == "" {
			log.Error("cashfree poll refund missing order_id; cannot verify gateway status, leaving initiated",
				"refund_id", refundID, "payment_id", ref.PaymentID)
			continue
		}
		details, err := j.RefundClient.FetchRefundStatus(ctx, orderID, refundID)
		if err != nil {
			var apiErr *cashfree.APIError
			isNotFound := errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
			if isNotFound && ref.Status == "initiated" && time.Since(ref.CreatedAt) > 30*time.Minute && hasVerifiedOrderID {
				log.Warn("cashfree poll refund not found at gateway after grace period; marking failed",
					"order_id", orderID, "refund_id", refundID, "created_at", ref.CreatedAt)
				if reconciler, ok := j.Refunds.(RefundReconciler); ok {
					if recErr := reconciler.ReconcileRefundTransition(ctx, &ref, "failed"); recErr != nil {
						log.Error("cashfree poll reconcile failed refund error", "refund_id", refundID, "err", recErr)
					}
				}
			} else {
				log.Error("cashfree poll refund fetch", "order_id", orderID, "refund_id", refundID, "has_verified_order_id", hasVerifiedOrderID, "err", err)
			}
			continue
		}
		if details == nil {
			continue
		}
		newStatus := domain.NormalizeRefundStatus(details.RefundStatus)
		if newStatus != ref.Status {
			if reconciler, ok := j.Refunds.(RefundReconciler); ok {
				if err := reconciler.ReconcileRefundTransition(ctx, &ref, newStatus); err != nil {
					log.Error("cashfree poll reconcile refund failed", "refund_id", refundID, "status", newStatus, "err", err)
				}
			} else {
				ref.Status = newStatus
				ref.UpdatedAt = time.Now().UTC()
				if err := j.Refunds.CreateOrUpdateRefund(ctx, &ref); err != nil {
					log.Error("cashfree poll update refund failed", "refund_id", refundID, "status", newStatus, "err", err)
				}
			}
			if newStatus == "succeeded" && j.Referrals != nil {
				if dueLister, ok := j.Refunds.(RefundDueLister); ok {
					if dueIDs, err := dueLister.GetRefundDueIDs(ctx, ref.ID); err == nil {
						for _, dueID := range dueIDs {
							if dueID != uuid.Nil {
								if err := j.Referrals.ReverseFirstRentPayment(ctx, dueID, ref.PaymentID); err != nil {
									log.Warn("cashfree poll referral reverse failed", "due_id", dueID, "payment_id", ref.PaymentID, "err", err)
								}
							}
						}
					}
				}
			}
		}
	}
	return nil
}
