package payment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

// OrderGatewaySettler settles a captured provider order canonically and idempotently.
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

// GatewayPaymentRepository provides persistence for gateway payments, audit events, and allocations.
type GatewayPaymentRepository interface {
	RecordProcessedEvent(ctx context.Context, provider, eventType, referenceID, payloadHash string) (bool, error)
	RecordUnmatchedReceipt(ctx context.Context, orderID, cfPaymentID string, intentID *uuid.UUID, amountPaise int64, reason string, payload []byte) error
	Create(ctx context.Context, p *domain.Payment) error
	CreateAllocation(ctx context.Context, paymentID, dueID uuid.UUID, amountPaise int64) error
}

// PaymentIntentRepository loads and updates payment intents and snapshot dues.
type PaymentIntentRepository interface {
	GetByOrderID(ctx context.Context, orderID string) (*domain.PaymentIntent, error)
	GetByCFPaymentID(ctx context.Context, cfPaymentID string) (*domain.PaymentIntent, error)
	GetDuesSnapshot(ctx context.Context, intentID uuid.UUID) ([]domain.PaymentIntentDue, error)
	MarkPaid(ctx context.Context, id uuid.UUID, cfPaymentID string) error
}

// LedgerOutboxInserter writes outbox events within a transaction.
type LedgerOutboxInserter interface {
	InsertLedgerOutboxEventTx(ctx context.Context, tx pgx.Tx, evt *domain.LedgerOutboxEvent) error
}

// GatewayPaymentSettler settles a due against a gateway transaction.
type GatewayPaymentSettler interface {
	GatewaySettle(ctx context.Context, dueID uuid.UUID, amountPaise int64, bankRef string, dedupKey ...string) (*domain.Payment, error)
}

// OrderSettlementConfig configures OrderSettlementService.
type OrderSettlementConfig struct {
	Pool               *pgxpool.Pool
	GatewayPaymentRepo GatewayPaymentRepository
	IntentStore        PaymentIntentRepository
	LedgerOutboxRepo   LedgerOutboxInserter
	FinanceEnabled     bool
	Payments           GatewayPaymentSettler
}

// OrderSettlementService performs canonical, idempotent settlement for gateway orders.
// Both HTTP webhooks and background polling workers invoke this service to prevent divergence.
type OrderSettlementService struct {
	pool               *pgxpool.Pool
	gatewayPaymentRepo GatewayPaymentRepository
	intentStore        PaymentIntentRepository
	ledgerOutboxRepo   LedgerOutboxInserter
	financeEnabled     bool
	payments           GatewayPaymentSettler
}

// NewOrderSettlementService constructs an OrderSettlementService.
func NewOrderSettlementService(cfg OrderSettlementConfig) *OrderSettlementService {
	return &OrderSettlementService{
		pool:               cfg.Pool,
		gatewayPaymentRepo: cfg.GatewayPaymentRepo,
		intentStore:        cfg.IntentStore,
		ledgerOutboxRepo:   cfg.LedgerOutboxRepo,
		financeEnabled:     cfg.FinanceEnabled,
		payments:           cfg.Payments,
	}
}

// SettleGatewayOrder executes canonical, idempotent settlement for a gateway payment.
// It enforces top-down hierarchical locking (tenants -> dues -> payment_intents),
// natural key deduplication, snapshot-based multi-due allocation, unapplied payment handling,
// and ledger outbox mirroring.
func (s *OrderSettlementService) SettleGatewayOrder(
	ctx context.Context,
	orderID string,
	cfPaymentID string,
	txnID string,
	amountPaise int64,
	intent *domain.PaymentIntent,
	rawPayload []byte,
) error {
	if strings.TrimSpace(cfPaymentID) == "" {
		return errors.New("settle payment failed: cf_payment_id cannot be blank")
	}
	if intent == nil && s.intentStore != nil {
		var err error
		intent, err = s.intentStore.GetByOrderID(ctx, orderID)
		if err != nil {
			return err
		}
	}
	if intent == nil {
		return errors.New("payment intent unavailable")
	}

	if intent.AmountPaise > 0 && amountPaise != intent.AmountPaise {
		if s.gatewayPaymentRepo != nil {
			_ = s.gatewayPaymentRepo.RecordUnmatchedReceipt(ctx, orderID, cfPaymentID, &intent.ID, amountPaise, "amount_mismatch", rawPayload)
		}
		return fmt.Errorf("settle amount mismatch: expected %d paise, got %d paise", intent.AmountPaise, amountPaise)
	}

	if s.pool != nil && s.gatewayPaymentRepo != nil {
		return postgres.WithinTx(ctx, s.pool, func(tx pgx.Tx) error {
			txPayRepo := postgres.NewPaymentRepo(tx)
			txDueRepo := postgres.NewDueRepo(tx)
			txIntentRepo := postgres.NewPaymentIntentRepo(tx)

			// Step 1: In-transaction deduplication on natural key (PAYMENT_SETTLED, cfPaymentID)
			firstSeen, err := txPayRepo.RecordProcessedEvent(ctx, "cashfree", "PAYMENT_SETTLED", cfPaymentID, "")
			if err != nil {
				return err
			}
			if !firstSeen {
				return nil // already processed by either webhook, verify, or poller
			}

			// Step 2: Dues snapshot from payment_intent_dues
			snapshot, err := txIntentRepo.GetDuesSnapshot(ctx, intent.ID)
			if err != nil {
				return fmt.Errorf("get dues snapshot: %w", err)
			}
			if len(snapshot) == 0 {
				snapshot = []domain.PaymentIntentDue{{DueID: intent.DueID, AmountPaise: int64(intent.AmountPaise)}}
			}

			// Step 3: Resolve first due to determine tenantID and propertyID
			due0, err := txDueRepo.GetByID(ctx, snapshot[0].DueID)
			if err != nil {
				return err
			}
			tenantID := due0.TenantID
			ctx = requestscope.WithPropertyID(ctx, due0.PropertyID)
			if _, err := tx.Exec(ctx, "SELECT set_config('app.current_property_id', $1, true)", due0.PropertyID.String()); err != nil {
				return fmt.Errorf("set settlement property scope: %w", err)
			}

			// Step 4: Acquire locks strictly top-down: tenants -> dues -> payment_intents
			var dummy uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id=$1 FOR UPDATE`, tenantID).Scan(&dummy); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}

			allClosed := true
			lockedDues := make(map[uuid.UUID]*domain.Due)
			for _, item := range snapshot {
				d, err := txDueRepo.GetByIDForUpdate(ctx, item.DueID)
				if err != nil {
					return err
				}
				lockedDues[d.ID] = d
				if d.Status == domain.DueStatusPending || d.Status == domain.DueStatusPartial {
					allClosed = false
				}
			}

			var lockedIntent domain.PaymentIntent
			err = tx.QueryRow(ctx, `SELECT id, status FROM payment_intents WHERE id=$1 FOR UPDATE`, intent.ID).Scan(&lockedIntent.ID, &lockedIntent.Status)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if lockedIntent.Status == domain.IntentPaid {
				// If this exact payment was already recorded, return idempotently.
				var existingCount int
				if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM payments WHERE cf_payment_id = $1 OR (upi_txn_id IS NOT NULL AND upi_txn_id = $2)`, cfPaymentID, txnID).Scan(&existingCount); err == nil && existingCount > 0 {
					return nil
				}
				// Otherwise, this is a distinct additional successful capture for an already-paid order/intent.
				// Route it to the unapplied branch instead of silently dropping it or double-allocating dues.
				allClosed = true
			}

			at := time.Now().UTC()

			if allClosed {
				// UNAPPLIED / RACE LOSER BRANCH (e.g. cash paid offline or duplicate payment)
				var existingCount int
				if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM payments WHERE cf_payment_id = $1 OR (upi_txn_id IS NOT NULL AND upi_txn_id = $2)`, cfPaymentID, txnID).Scan(&existingCount); err == nil && existingCount > 0 {
					return nil
				}

				failureReason := "due_already_settled"
				if lockedIntent.Status == domain.IntentPaid {
					failureReason = "duplicate_payment"
				} else if lockedIntent.Status == "superseded" {
					failureReason = "session_superseded"
				}
				if err := txPayRepo.RecordUnmatchedReceipt(ctx, orderID, cfPaymentID, &intent.ID, amountPaise, failureReason, rawPayload); err != nil {
					return err
				}

				note := "unapplied duplicate: dues already closed, pending owner refund or manual allocation"
				if lockedIntent.Status == domain.IntentPaid {
					note = "unapplied duplicate capture: order was already paid, held for owner review/refund"
				}
				p := &domain.Payment{
					DueID:             snapshot[0].DueID,
					TenantID:          tenantID,
					UPITxnID:          &txnID,
					CFPaymentID:       &cfPaymentID,
					Amount:            amountPaise,
					MatchedBy:         domain.MatchedByCashfree,
					MatchedAt:         at,
					IsUnapplied:       true,
					RawNote:           &note,
					SkipOutboxEnqueue: true,
				}
				if err := txPayRepo.Create(ctx, p); err != nil {
					if errors.Is(err, domain.ErrDuplicateTxn) {
						return nil
					}
					return err
				}

				if err := txIntentRepo.MarkPaid(ctx, intent.ID, cfPaymentID); err != nil {
					return err
				}
				if s.financeEnabled && s.ledgerOutboxRepo != nil {
					payload, err := json.Marshal(domain.PaymentMirrorPayload{
						PropertyID:  due0.PropertyID,
						PaymentID:   p.ID,
						IsUnapplied: true,
						MatchedAt:   p.MatchedAt,
						MatchedBy:   p.MatchedBy,
						AmountPaise: int64(p.Amount),
					})
					if err != nil {
						return err
					}
					outboxEvt := &domain.LedgerOutboxEvent{
						EventType:      "payment_mirror",
						PropertyID:     due0.PropertyID,
						SourceID:       p.ID,
						Payload:        payload,
						IdempotencyKey: fmt.Sprintf("payment_mirror:%s", p.ID.String()),
						MaxAttempts:    5,
					}
					if err := s.ledgerOutboxRepo.InsertLedgerOutboxEventTx(ctx, tx, outboxEvt); err != nil {
						return err
					}
				}
				return nil
			}

			// APPLIED TO OPEN DUES BRANCH
			var existingCount int
			if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM payments WHERE cf_payment_id = $1`, cfPaymentID).Scan(&existingCount); err == nil && existingCount > 0 {
				return nil
			}

			p := &domain.Payment{
				DueID:             snapshot[0].DueID,
				TenantID:          tenantID,
				UPITxnID:          &txnID,
				CFPaymentID:       &cfPaymentID,
				Amount:            amountPaise,
				MatchedBy:         domain.MatchedByCashfree,
				MatchedAt:         at,
				IsUnapplied:       false,
				SkipOutboxEnqueue: true,
			}
			if err := txPayRepo.Create(ctx, p); err != nil {
				if errors.Is(err, domain.ErrDuplicateTxn) {
					return nil
				}
				return err
			}

			remainingPaise := amountPaise
			type allocItem struct {
				AmountPaise int64
				DueKind     domain.DueKind
			}
			var allocations []allocItem
			for _, item := range snapshot {
				if remainingPaise <= 0 {
					break
				}
				targetDue := lockedDues[item.DueID]
				if targetDue.Status != domain.DueStatusPending && targetDue.Status != domain.DueStatusPartial {
					continue
				}
				allocPaise := int64(targetDue.Amount)
				if allocPaise > remainingPaise {
					allocPaise = remainingPaise
				}
				if err := txPayRepo.CreateAllocation(ctx, p.ID, targetDue.ID, allocPaise); err != nil {
					return err
				}
				allocations = append(allocations, allocItem{
					AmountPaise: allocPaise,
					DueKind:     targetDue.Kind,
				})
				credit := targetDue.ApplyPayment(allocPaise, at)
				if err := txDueRepo.Update(ctx, targetDue); err != nil {
					return err
				}
				if targetDue.Status == domain.DueStatusPaid {
					if _, err := tx.Exec(ctx, `UPDATE payment_tokens SET used=TRUE WHERE due_id=$1 AND used=FALSE`, targetDue.ID); err != nil {
						return err
					}
				}
				if credit > 0 {
					if _, err := tx.Exec(ctx, `UPDATE tenants SET credit_balance_paise = credit_balance_paise + $2 WHERE id=$1`, tenantID, credit); err != nil {
						return err
					}
				}
				remainingPaise -= allocPaise
			}
			unappliedPaise := remainingPaise
			if unappliedPaise > 0 {
				if _, err := tx.Exec(ctx, `UPDATE tenants SET credit_balance_paise = credit_balance_paise + $2 WHERE id=$1`, tenantID, unappliedPaise); err != nil {
					return err
				}
			}

			if err := txIntentRepo.MarkPaid(ctx, intent.ID, cfPaymentID); err != nil {
				return err
			}
			if s.financeEnabled && s.ledgerOutboxRepo != nil {
				allocPayloads := make([]domain.PaymentAllocationItemPayload, 0, len(allocations))
				for _, a := range allocations {
					allocPayloads = append(allocPayloads, domain.PaymentAllocationItemPayload{
						AmountPaise: a.AmountPaise,
						DueKind:     string(a.DueKind),
					})
				}
				if len(allocPayloads) == 0 {
					allocPayloads = append(allocPayloads, domain.PaymentAllocationItemPayload{
						AmountPaise: int64(p.Amount),
						DueKind:     string(due0.Kind),
					})
				}
				payload, err := json.Marshal(domain.PaymentMirrorPayload{
					PropertyID:     due0.PropertyID,
					PaymentID:      p.ID,
					Allocations:    allocPayloads,
					UnappliedPaise: unappliedPaise,
					IsUnapplied:    false,
					MatchedAt:      p.MatchedAt,
					MatchedBy:      p.MatchedBy,
					AmountPaise:    int64(p.Amount),
				})
				if err != nil {
					return err
				}
				outboxEvt := &domain.LedgerOutboxEvent{
					EventType:      "payment_mirror",
					PropertyID:     due0.PropertyID,
					SourceID:       p.ID,
					Payload:        payload,
					IdempotencyKey: fmt.Sprintf("payment_mirror:%s", p.ID.String()),
					MaxAttempts:    5,
				}
				if err := s.ledgerOutboxRepo.InsertLedgerOutboxEventTx(ctx, tx, outboxEvt); err != nil {
					return err
				}
			}
			return nil
		})
	}

	// Fallback path for unit tests without a database pool
	if s.gatewayPaymentRepo != nil && cfPaymentID != "" {
		firstSeen, err := s.gatewayPaymentRepo.RecordProcessedEvent(ctx, "cashfree", "PAYMENT_SETTLED", cfPaymentID, "")
		if err != nil {
			return err
		}
		if !firstSeen {
			return nil
		}
	}
	if cfPaymentID != "" && s.intentStore != nil {
		if existing, err := s.intentStore.GetByCFPaymentID(ctx, cfPaymentID); err == nil && existing != nil && existing.Status == domain.IntentPaid {
			return nil
		}
	}
	if s.payments != nil {
		var duesToSettle []domain.PaymentIntentDue
		if s.intentStore != nil {
			duesToSettle, _ = s.intentStore.GetDuesSnapshot(ctx, intent.ID)
		}
		if len(duesToSettle) == 0 {
			duesToSettle = []domain.PaymentIntentDue{{DueID: intent.DueID, AmountPaise: amountPaise}}
		}
		var settledCount int
		for _, item := range duesToSettle {
			itemTxn := txnID
			if len(duesToSettle) > 1 {
				itemTxn = fmt.Sprintf("%s:due:%s", txnID, item.DueID)
			}
			dedupKey := ""
			if cfPaymentID != "" {
				if len(duesToSettle) > 1 {
					dedupKey = fmt.Sprintf("cashfree:pg:%s:due:%s", cfPaymentID, item.DueID)
				} else {
					dedupKey = fmt.Sprintf("cashfree:pg:%s", cfPaymentID)
				}
			}
			var err error
			if dedupKey != "" {
				_, err = s.payments.GatewaySettle(ctx, item.DueID, item.AmountPaise, itemTxn, dedupKey)
			} else {
				_, err = s.payments.GatewaySettle(ctx, item.DueID, item.AmountPaise, itemTxn)
			}
			if err != nil {
				if errors.Is(err, ErrDuplicateTxn) {
					settledCount++
					continue
				}
				if errors.Is(err, ErrDueNotOpen) || errors.Is(err, ErrEmptyTxnID) {
					continue
				}
				return err
			}
			settledCount++
		}
		if settledCount == 0 {
			return nil
		}
	}
	if cfPaymentID != "" && s.intentStore != nil {
		_ = s.intentStore.MarkPaid(ctx, intent.ID, cfPaymentID)
	}
	return nil
}
