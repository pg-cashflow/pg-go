package jobs

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

const CashfreePollStale = 10 * time.Minute

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
	GatewaySettle(ctx context.Context, dueID uuid.UUID, amountPaise int, txnID string) (*domain.Payment, error)
}

// CashfreePollJob settles created intents whose webhook was missed.
type CashfreePollJob struct {
	Intents    IntentStaleLister
	Dues       DueByID
	Client     CashfreeFetcher
	Settle     GatewaySettler
	StaleAfter time.Duration
	Now        func() time.Time
	Log        *slog.Logger
}

func (j *CashfreePollJob) Run(ctx context.Context) error {
	if j.Client == nil || j.Intents == nil {
		return nil
	}
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
		_, err = j.Settle.GatewaySettle(ctx, intent.DueID, amount, txn)
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
