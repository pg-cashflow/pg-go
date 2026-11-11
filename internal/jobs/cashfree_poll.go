package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/payment"
)

const (
	CashfreePollStale       = 10 * time.Minute
	CashfreeRefundPollStale = 2 * time.Minute
	// CashfreeRefundStuckAfter is the age after which a non-terminal refund is logged as a CRITICAL alert.
	CashfreeRefundStuckAfter = 72 * time.Hour
	CashfreePollLockName     = "cashfree_poll"
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

type OrderGatewaySettler interface {
	SettleGatewayOrder(
		ctx context.Context,
		orderID string,
		cfPaymentID string,
		txnID string,
		amountPaise int64,
		intent *domain.PaymentIntent,
		rawPayload []byte,
	) error
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

// RefundResubmitter extends RefundStatusFetcher to support automated resubmission
// for refunds that crashed before initial gateway dispatch.
type RefundResubmitter interface {
	RefundStatusFetcher
	CreateRefund(ctx context.Context, orderID, refundID string, amountPaise int64, reason, idempotencyKey string) (*cashfree.RefundDetails, error)
}

// RefundOperationLocker abstracts mutual exclusion for refund operations.
type RefundOperationLocker interface {
	TryLockRefundOperation(ctx context.Context, refundID uuid.UUID) (acquired bool, unlock func(), err error)
}

// CashfreePollJob settles created intents whose webhook was missed, and reconciles stuck refunds.
type CashfreePollJob struct {
	Intents          IntentStaleLister
	Dues             DueByID
	Client           CashfreeFetcher
	Settle           GatewaySettler
	OrderSettler     OrderGatewaySettler
	Refunds          RefundStaleRepo
	RefundClient     RefundStatusFetcher
	RefundLocker     RefundOperationLocker
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

// RunRefundsOnly executes the refund polling and reconciliation cycle for ambiguous non-terminal refunds.
func (j *CashfreePollJob) RunRefundsOnly(ctx context.Context) error {
	if j.Refunds != nil && j.RefundClient != nil {
		return j.runRefunds(ctx)
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
		if j.OrderSettler == nil && j.Dues != nil {
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

		if j.OrderSettler != nil {
			intentCopy := intent
			if err := j.OrderSettler.SettleGatewayOrder(ctx, intent.ProviderOrderID, cfID, txn, amount, &intentCopy, nil); err != nil {
				log.Error("cashfree poll settle order", "order", intent.ProviderOrderID, "err", err)
				continue
			}
		} else if j.Settle != nil {
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
	}
	return nil
}

var errPollRateLimited = errors.New("cashfree poll rate limited")

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
		if err := j.pollSingleRefund(ctx, now, ref, log); err != nil {
			if errors.Is(err, errPollRateLimited) {
				return nil
			}
		}
	}
	return nil
}

func (j *CashfreePollJob) pollSingleRefund(ctx context.Context, now time.Time, ref domain.GatewayRefund, log *slog.Logger) error {
	refundID := ""
	if ref.RefundReference != nil && *ref.RefundReference != "" {
		refundID = *ref.RefundReference
	} else if ref.CFRefundID != nil {
		refundID = *ref.CFRefundID
	}
	if refundID == "" {
		return nil
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
				hasVerifiedOrderID = true
			}
		}
	}
	if orderID == "" {
		log.Error("cashfree poll refund missing order_id; cannot verify gateway status, leaving initiated",
			"refund_id", refundID, "payment_id", ref.PaymentID)
		return nil
	}
	if now.Sub(ref.CreatedAt) > CashfreeRefundStuckAfter {
		log.Error("CRITICAL: refund non-terminal beyond stuck threshold; dues remain reserved, investigate at gateway",
			"refund_id", refundID, "payment_id", ref.PaymentID, "status", ref.Status, "age_hours", int(now.Sub(ref.CreatedAt).Hours()))
	}
	details, err := j.RefundClient.FetchRefundStatus(ctx, orderID, refundID)
	if err != nil {
		var apiErr *cashfree.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusTooManyRequests {
			// Rate limited: stop this run; the next scheduled run continues. Avoids hammering the gateway.
			log.Warn("cashfree poll refund rate limited; deferring remaining refunds to next run", "refund_id", refundID)
			return errPollRateLimited
		}
		isNotFound := errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
		if isNotFound && ref.Status == "initiated" && hasVerifiedOrderID {
			// The refund was not found at Cashfree (404). This can occur if the application
			// crashed after committing the initiated refund row locally, but before the HTTP
			// call to Cashfree completed. If the client implements RefundResubmitter and
			// the record is still within 24 hours, attempt automated idempotent resubmission.
			refundAge := now.Sub(ref.CreatedAt)
			if resubmitter, ok := j.RefundClient.(RefundResubmitter); ok && refundAge >= 0 && refundAge < 24*time.Hour {
				// R4: Acquire mutual exclusion lock before attempting resubmission
				if j.RefundLocker != nil {
					acquired, unlock, lockErr := j.RefundLocker.TryLockRefundOperation(ctx, ref.ID)
					if lockErr != nil || !acquired {
						log.Warn("cashfree poll: refund locked by concurrent operation, skipping resubmission", "refund_id", ref.ID)
						return nil
					}
					defer unlock()
				}

				log.Info("cashfree poll: resubmitting initiated refund not found at gateway",
					"order_id", orderID, "refund_id", refundID, "amount_paise", ref.AmountPaise)
				// R6: Gateway confirmed the refund is absent. Rotate the idempotency key (a repeated key
				// replays the cached first result); refund_id stays fixed and prevents duplicates.
				retryIdempotencyKey := domain.CashfreeRefundRecoveryIdempotencyKey(ref.ID, time.Now())
				refundNote := fmt.Sprintf("Refund %s", refundID)
				if len(ref.RawPayload) > 0 {
					var rawMap map[string]any
					if err := json.Unmarshal(ref.RawPayload, &rawMap); err == nil {
						if n, ok := rawMap["refund_note"].(string); ok && strings.TrimSpace(n) != "" {
							refundNote = strings.TrimSpace(n)
						}
					}
				}
				if refundNote == fmt.Sprintf("Refund %s", refundID) {
					if detector, ok := j.RefundClient.(interface{ IsSandbox() bool }); ok && detector.IsSandbox() {
						upperReason := strings.ToUpper(ref.Reason)
						var sandboxOutcome string
						switch {
						case strings.Contains(upperReason, "FAILED"):
							sandboxOutcome = "FAILED"
						case strings.Contains(upperReason, "PENDING"):
							sandboxOutcome = "PENDING"
						case strings.Contains(upperReason, "SUCCESS"):
							sandboxOutcome = "SUCCESS"
						}
						if sandboxOutcome != "" {
							refundNote = fmt.Sprintf("Refund %s %s", refundID, sandboxOutcome)
						}
					}
				}
				createdDetails, createErr := resubmitter.CreateRefund(ctx, orderID, refundID, ref.AmountPaise, refundNote, retryIdempotencyKey)
				if createErr == nil && createdDetails != nil {
					if valErr := cashfree.ValidateRefundResponse(orderID, refundID, ref.AmountPaise, createdDetails); valErr != nil {
						log.Error("cashfree poll: resubmitted refund response validation failed", "refund_id", refundID, "err", valErr)
						return nil
					}
					details = createdDetails
					err = nil
				} else if createErr != nil {
					var createApiErr *cashfree.APIError
					if errors.As(createErr, &createApiErr) {
						// If Cashfree reports the refund already exists (e.g. 409 or 400 refund_already_exists),
						// do not mark failed. Re-query status from gateway.
						isDuplicate := cashfree.IsRefundDuplicateError(createErr)

						if isDuplicate {
							log.Info("cashfree poll: resubmitted refund already exists at gateway; re-fetching status",
								"order_id", orderID, "refund_id", refundID)
							if fetched, fetchErr := j.RefundClient.FetchRefundStatus(ctx, orderID, refundID); fetchErr == nil && fetched != nil {
								details = fetched
								err = nil
								createErr = nil
							} else {
								return nil
							}
						} else if createApiErr.StatusCode == http.StatusUnauthorized || createApiErr.StatusCode == http.StatusForbidden {
							// Authentication/permission failure is a gateway config error, not a refund rejection.
							log.Error("cashfree poll: resubmit refund authentication rejected (check credentials/IP)",
								"order_id", orderID, "refund_id", refundID, "status", createApiErr.StatusCode)
							return nil
						} else if createApiErr.StatusCode >= 400 && createApiErr.StatusCode < 500 && createApiErr.StatusCode != http.StatusTooManyRequests {
							log.Warn("cashfree poll: resubmitted refund rejected definitively; marking failed",
								"order_id", orderID, "refund_id", refundID, "err", createErr)
							if reconciler, ok := j.Refunds.(RefundReconciler); ok {
								if recErr := reconciler.ReconcileRefundTransition(ctx, &ref, "failed"); recErr != nil {
									log.Error("cashfree poll reconcile failed refund error", "refund_id", refundID, "err", recErr)
								}
							}
							return nil
						}
					}
					if createErr != nil {
						log.Error("cashfree poll: resubmit refund temporary error", "order_id", orderID, "refund_id", refundID, "err", createErr)
						return nil
					}
				}
			} else if refundAge > 30*time.Minute {
				log.Warn("cashfree poll refund not found at gateway after grace period; marking needs_review for manual review",
					"order_id", orderID, "refund_id", refundID, "created_at", ref.CreatedAt)
				if reconciler, ok := j.Refunds.(RefundReconciler); ok {
					if recErr := reconciler.ReconcileRefundTransition(ctx, &ref, domain.RefundStatusNeedsReview); recErr != nil {
						log.Error("cashfree poll reconcile needs_review refund error", "refund_id", refundID, "err", recErr)
					}
				}
				return nil
			} else {
				log.Info("cashfree poll: refund not yet visible at gateway; waiting for grace window",
					"order_id", orderID, "refund_id", refundID)
				return nil
			}
		} else {
			log.Error("cashfree poll refund fetch", "order_id", orderID, "refund_id", refundID, "has_verified_order_id", hasVerifiedOrderID, "err", err)
			return nil
		}
	}
	if details == nil {
		return nil
	}
	if details.AmountPaise <= 0 || details.AmountPaise != ref.AmountPaise {
		log.Error("cashfree poll refund amount mismatch or missing/non-positive; leaving initiated for manual investigation",
			"order_id", orderID, "refund_id", refundID, "gateway_amount", details.AmountPaise, "local_amount", ref.AmountPaise)
		return nil
	}
	if details.CFRefundID != "" && (ref.CFRefundID == nil || *ref.CFRefundID == "") {
		ref.CFRefundID = &details.CFRefundID
		ref.ProviderRefundID = &details.CFRefundID
	}
	newStatus := domain.NormalizeRefundStatus(details.RefundStatus)
	if newStatus == domain.RefundStatusOnHold {
		log.Warn("cashfree refund is ONHOLD at gateway; merchant account requires balance top-up",
			"order_id", orderID, "refund_id", refundID, "amount_paise", ref.AmountPaise)
	}
	var reconcileErr error
	if newStatus != ref.Status {
		if reconciler, ok := j.Refunds.(RefundReconciler); ok {
			if err := reconciler.ReconcileRefundTransition(ctx, &ref, newStatus); err != nil {
				reconcileErr = err
				log.Error("cashfree poll reconcile refund failed", "refund_id", refundID, "status", newStatus, "err", err)
			}
		} else {
			ref.Status = newStatus
			ref.UpdatedAt = time.Now().UTC()
			if err := j.Refunds.CreateOrUpdateRefund(ctx, &ref); err != nil {
				reconcileErr = err
				log.Error("cashfree poll update refund failed", "refund_id", refundID, "status", newStatus, "err", err)
			}
		}
	}
	if reconcileErr == nil && (newStatus == "succeeded" || ref.Status == "succeeded") && j.Referrals != nil {
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
	return nil
}
