package jobs

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	FetchSuccessfulPayment(ctx context.Context, orderID string) (cfPaymentID, bankRef string, amountPaise int, ok bool, err error)
}

type GatewaySettler interface {
	GatewaySettle(ctx context.Context, dueID uuid.UUID, amountPaise int, txnID string, dedupKey ...string) (*domain.Payment, error)
}

type RefundStaleRepo interface {
	ListStaleNonTerminalRefunds(ctx context.Context, olderThan time.Time) ([]domain.GatewayRefund, error)
	CreateOrUpdateRefund(ctx context.Context, ref *domain.GatewayRefund) error
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
		details, err := j.RefundClient.FetchRefundStatus(ctx, "", refundID)
		if err != nil {
			log.Error("cashfree poll refund fetch", "refund_id", refundID, "err", err)
			continue
		}
		if details == nil {
			continue
		}
		newStatus := strings.ToLower(details.RefundStatus)
		if newStatus != ref.Status {
			ref.Status = newStatus
			ref.UpdatedAt = time.Now().UTC()
			_ = j.Refunds.CreateOrUpdateRefund(ctx, &ref)
		}
	}
	return nil
}
