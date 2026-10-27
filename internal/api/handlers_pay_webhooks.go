package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/finance"
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

const maxWebhookBody = 1 << 20

// CashfreeWebhook handles POST /webhooks/cashfree.
func (h *Handlers) CashfreeWebhook(c *gin.Context) {
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, maxWebhookBody+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "body"})
		return
	}
	if len(raw) > maxWebhookBody {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "body too large"})
		return
	}
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))

	// Fail closed if CashfreeSecret is unconfigured
	if h.CashfreeSecret == "" {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "payment gateway not configured"})
		return
	}

	sig := c.GetHeader("x-webhook-signature")
	ts := c.GetHeader("x-webhook-timestamp")
	if !cashfree.VerifyWebhookHMAC(h.CashfreeSecret, ts, string(raw), sig) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}

	// Verify timestamp tolerance (drift check) with fail-closed default (300s)
	tolerance := h.WebhookToleranceSec
	if tolerance <= 0 {
		tolerance = 300
	}
	if err := cashfree.VerifyWebhookTimestamp(ts, tolerance, time.Now().UTC()); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "timestamp tolerance exceeded"})
		return
	}

	// Ingest raw arrival into webhook_events audit log
	evtRecord := &domain.WebhookEvent{
		Provider:         "cashfree",
		Signature:        &sig,
		TimestampHeader:  &ts,
		RawPayload:       raw,
		ProcessingStatus: "received",
	}
	if h.GatewayPaymentRepo == nil {
		slog.Default().Error("webhook processing rejected: GatewayPaymentRepo is nil (audit log unavailable)")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "audit log service unavailable"})
		return
	}
	if err := h.GatewayPaymentRepo.CreateWebhookEvent(c.Request.Context(), evtRecord); err != nil {
		slog.Default().Error("webhook forensic audit persistence failed", "err", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "audit log persistence failed"})
		return
	}

	val, evtType, err := cashfree.ParseWebhook(raw)
	if err != nil {
		// DEAD-LETTER ISOLATION: mark error, alert owner, return 200 OK to stop gateway retry storms
		errMsg := err.Error()
		if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(c.Request.Context(), evtRecord.ID, "dead_letter", &errMsg)
		}
		slog.Default().Error("cashfree webhook dead-letter (unparseable payload)", "err", err)
		c.Status(http.StatusOK)
		return
	}

	evtRecord.EventType = evtType

	switch evtType {
	case "PAYMENT_SUCCESS_WEBHOOK":
		succ, ok := val.(cashfree.SuccessWebhook)
		if !ok {
			c.Status(http.StatusOK)
			return
		}
		h.handlePaymentSuccessWebhook(c, succ, evtRecord)

	case "PAYMENT_FAILED_WEBHOOK":
		fail, ok := val.(cashfree.FailedWebhook)
		if !ok {
			c.Status(http.StatusOK)
			return
		}
		h.handlePaymentFailedWebhook(c, fail, evtRecord)

	case "REFUND_STATUS_WEBHOOK", "AUTO_REFUND_STATUS_WEBHOOK":
		ref, ok := val.(cashfree.RefundWebhook)
		if !ok {
			c.Status(http.StatusOK)
			return
		}
		h.handleRefundWebhook(c, ref, evtRecord)

	case "DISPUTE_CREATED_WEBHOOK", "PAYMENT_DISPUTE_CREATED_WEBHOOK", "DISPUTE_STATUS_UPDATE_WEBHOOK":
		disp, ok := val.(cashfree.DisputeWebhook)
		if !ok {
			c.Status(http.StatusOK)
			return
		}
		h.handleDisputeWebhook(c, disp, evtRecord)

	default:
		if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(c.Request.Context(), evtRecord.ID, "ignored", nil)
		}
		c.Status(http.StatusOK)
	}
}

func (h *Handlers) handlePaymentSuccessWebhook(c *gin.Context, succ cashfree.SuccessWebhook, evtRecord *domain.WebhookEvent) {
	ctx := c.Request.Context()
	if h.IntentStore == nil {
		c.Status(http.StatusOK)
		return
	}

	intent, err := h.IntentStore.GetByOrderID(ctx, succ.OrderID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			if h.GatewayPaymentRepo != nil {
				if err := h.GatewayPaymentRepo.RecordUnmatchedReceipt(ctx, succ.OrderID, succ.CFPaymentID, nil, int64(succ.AmountPaise), "unknown_order", evtRecord.RawPayload); err != nil {
					slog.Error("failed recording unmatched receipt", "order_id", succ.OrderID, "cf_payment_id", succ.CFPaymentID, "err", err)
					c.JSON(http.StatusInternalServerError, gin.H{"error": "record unmatched receipt failed"})
					return
				}
			}
			if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
				_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "unmatched", nil)
			}
			c.Status(http.StatusOK)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "lookup failed"})
		return
	}

	if succ.AmountPaise != intent.AmountPaise {
		if h.GatewayPaymentRepo != nil {
			if err := h.GatewayPaymentRepo.RecordUnmatchedReceipt(ctx, succ.OrderID, succ.CFPaymentID, &intent.ID, int64(succ.AmountPaise), "amount_mismatch", evtRecord.RawPayload); err != nil {
				slog.Error("failed recording unmatched receipt for amount mismatch", "order_id", succ.OrderID, "err", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "record unmatched receipt failed"})
				return
			}
		}
		c.Status(http.StatusOK)
		return
	}

	txnID := succ.TxnID()
	if err := h.SettleGatewayOrder(ctx, succ.OrderID, succ.CFPaymentID, txnID, succ.AmountPaise, intent, evtRecord.RawPayload); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settle failed"})
		return
	}

	if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
		_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "processed", nil)
	}
	c.Status(http.StatusOK)
}

// SettleGatewayOrder executes canonical, idempotent settlement for a gateway payment.
// It is shared by both CashfreeWebhook and TenantDuePayVerify to guarantee identical locking,
// deduplication, multi-due allocation, and ledger outbox mirroring in both execution orders.
func (h *Handlers) SettleGatewayOrder(
	ctx context.Context,
	orderID string,
	cfPaymentID string,
	txnID string,
	amountPaise int64,
	intent *domain.PaymentIntent,
	rawPayload []byte,
) error {
	if intent == nil && h.IntentStore != nil {
		var err error
		intent, err = h.IntentStore.GetByOrderID(ctx, orderID)
		if err != nil {
			return err
		}
	}
	if intent == nil {
		return errors.New("payment intent unavailable")
	}

	if amountPaise != intent.AmountPaise {
		if h.GatewayPaymentRepo != nil {
			_ = h.GatewayPaymentRepo.RecordUnmatchedReceipt(ctx, orderID, cfPaymentID, &intent.ID, amountPaise, "amount_mismatch", rawPayload)
		}
		return fmt.Errorf("settle amount mismatch: expected %d paise, got %d paise", intent.AmountPaise, amountPaise)
	}

	if h.Pool != nil && h.GatewayPaymentRepo != nil {
		return postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
			txPayRepo := postgres.NewPaymentRepo(tx)
			txDueRepo := postgres.NewDueRepo(tx)
			txIntentRepo := postgres.NewPaymentIntentRepo(tx)

			// Step 1: In-transaction deduplication on natural key (PAYMENT_SETTLED, cfPaymentID)
			firstSeen, err := txPayRepo.RecordProcessedEvent(ctx, "cashfree", "PAYMENT_SETTLED", cfPaymentID, "")
			if err != nil {
				return err
			}
			if !firstSeen {
				return nil // already processed by either webhook or verify
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
				return fmt.Errorf("set webhook property scope: %w", err)
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
				return nil // already paid
			}

			at := time.Now().UTC()

			if allClosed {
				// UNAPPLIED / RACE LOSER BRANCH (e.g. cash paid offline or duplicate payment)
				// Guard against duplicate insert if payment was already recorded
				var existingCount int
				if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM payments WHERE cf_payment_id = $1 OR (upi_txn_id IS NOT NULL AND upi_txn_id = $2)`, cfPaymentID, txnID).Scan(&existingCount); err == nil && existingCount > 0 {
					return nil
				}

				failureReason := "due_already_settled"
				if lockedIntent.Status == "superseded" {
					failureReason = "session_superseded"
				}
				if err := txPayRepo.RecordUnmatchedReceipt(ctx, orderID, cfPaymentID, &intent.ID, amountPaise, failureReason, rawPayload); err != nil {
					return err
				}

				note := "unapplied duplicate: dues already closed, pending owner refund or manual allocation"
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
				if h.Finance != nil {
					if h.LedgerOutboxRepo == nil {
						return fmt.Errorf("CRITICAL: LedgerOutboxRepo is required when Finance is enabled")
					}
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
					if err := h.LedgerOutboxRepo.InsertLedgerOutboxEventTx(ctx, tx, outboxEvt); err != nil {
						return err
					}
				}
				return nil
			}

			// APPLIED TO OPEN DUES BRANCH
			// Guard against duplicate insert if payment was already recorded
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
			var allocations []finance.PaymentAllocationItem
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
				allocations = append(allocations, finance.PaymentAllocationItem{
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
			if h.Finance != nil {
				if h.LedgerOutboxRepo == nil {
					return fmt.Errorf("CRITICAL: LedgerOutboxRepo is required when Finance is enabled")
				}
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
				if err := h.LedgerOutboxRepo.InsertLedgerOutboxEventTx(ctx, tx, outboxEvt); err != nil {
					return err
				}
			}
			return nil
		})
	}

	// Legacy fallback path for test stubs without a pgxpool
	if h.GatewayPaymentRepo != nil && cfPaymentID != "" {
		firstSeen, err := h.GatewayPaymentRepo.RecordProcessedEvent(ctx, "cashfree", "PAYMENT_SETTLED", cfPaymentID, "")
		if err != nil {
			return err
		}
		if !firstSeen {
			return nil // already processed
		}
	}
	if cfPaymentID != "" && h.IntentStore != nil {
		if existing, err := h.IntentStore.GetByCFPaymentID(ctx, cfPaymentID); err == nil && existing != nil && existing.Status == domain.IntentPaid {
			return nil
		}
	}
	if h.Payments != nil {
		dedupKey := ""
		if cfPaymentID != "" {
			dedupKey = fmt.Sprintf("cashfree:pg:%s", cfPaymentID)
		}
		var err error
		if dedupKey != "" {
			_, err = h.Payments.GatewaySettle(ctx, intent.DueID, amountPaise, txnID, dedupKey)
		} else {
			_, err = h.Payments.GatewaySettle(ctx, intent.DueID, amountPaise, txnID)
		}
		if err != nil {
			if errors.Is(err, payment.ErrDuplicateTxn) || errors.Is(err, payment.ErrDueNotOpen) || errors.Is(err, payment.ErrEmptyTxnID) {
				return nil
			}
			return err
		}
	}
	if cfPaymentID != "" && h.IntentStore != nil {
		_ = h.IntentStore.MarkPaid(ctx, intent.ID, cfPaymentID)
	}
	return nil
}

func (h *Handlers) handlePaymentFailedWebhook(c *gin.Context, fail cashfree.FailedWebhook, evtRecord *domain.WebhookEvent) {
	ctx := c.Request.Context()
	if h.Pool != nil && h.GatewayPaymentRepo != nil {
		if txErr := postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
			txPayRepo := postgres.NewPaymentRepo(tx)
			firstSeen, err := txPayRepo.RecordProcessedEvent(ctx, "cashfree", "PAYMENT_FAILED_WEBHOOK", fail.CFPaymentID, "")
			if err != nil || !firstSeen {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE payment_intents SET status='failed', updated_at=NOW() WHERE provider_order_id=$1 AND status NOT IN ('paid', 'refunded')`, fail.OrderID); err != nil {
				return err
			}
			return nil
		}); txErr != nil {
			// Transaction failure on payment-failed webhook — the intent status update or dedup insert failed.
			// Log at ERROR for operator visibility; still return 200 OK to prevent gateway retry storms on
			// a non-money-moving event. The webhook_events audit record is the authoritative receipt.
			slog.Default().Error("PAYMENT_FAILED_WEBHOOK: transaction error updating payment_intents",
				"order_id", fail.OrderID,
				"cf_payment_id", fail.CFPaymentID,
				"err", txErr,
			)
		}
	}
	if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
		_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "processed", nil)
	}
	c.Status(http.StatusOK)
}

func (h *Handlers) handleRefundWebhook(c *gin.Context, ref cashfree.RefundWebhook, evtRecord *domain.WebhookEvent) {
	ctx := c.Request.Context()
	if h.GatewayPaymentRepo == nil {
		c.Status(http.StatusOK)
		return
	}

	// Step 1: Unlocked ancestry resolution
	var existingRef *domain.GatewayRefund
	if ref.RefundID != "" {
		existingRef, _ = h.GatewayPaymentRepo.GetRefundByReference(ctx, ref.RefundID)
	}
	if existingRef == nil && ref.CFRefundID != "" {
		existingRef, _ = h.GatewayPaymentRepo.GetRefundByCFRefundID(ctx, ref.CFRefundID)
	}
	var p *domain.Payment
	if existingRef != nil {
		p, _ = h.GatewayPaymentRepo.GetByID(ctx, existingRef.PaymentID)
	} else if ref.CFPaymentID != "" {
		p, _ = h.GatewayPaymentRepo.GetByCFPaymentID(ctx, ref.CFPaymentID)
	}

	if p == nil {
		if err := h.GatewayPaymentRepo.RecordUnmatchedReceipt(ctx, ref.OrderID, ref.CFPaymentID, nil, ref.RefundAmount, "payment_not_found", evtRecord.RawPayload); err != nil {
			slog.Error("failed recording unmatched receipt for refund", "order_id", ref.OrderID, "err", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "record unmatched receipt failed"})
			return
		}
		if evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "unmatched", nil)
		}
		c.Status(http.StatusOK)
		return
	}

	if h.DueStore != nil && p.DueID != uuid.Nil {
		_, err := h.DueStore.GetByID(ctx, p.DueID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "due lookup failed"})
			return
		}
	}

	// Step 2: Lock hierarchy and forward-only status transition
	if h.Pool != nil {
		var committedDueIDs []uuid.UUID

		err := postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
			txPayRepo := postgres.NewPaymentRepo(tx)
			txDueRepo := postgres.NewDueRepo(tx)

			// In-transaction deduplication
			firstSeen, err := txPayRepo.RecordProcessedEvent(ctx, "cashfree", ref.Type, ref.CFRefundID, ref.RefundStatus)
			if err != nil {
				return err
			}
			if !firstSeen {
				return nil
			}

			// Locks: tenants -> dues -> payments -> gateway_refunds
			// Errors on FOR UPDATE lock rows must propagate — if the row is missing or
			// the query fails, proceeding without the lock breaks the lock hierarchy.
			var dummy uuid.UUID
			if lockErr := tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id=$1 FOR UPDATE`, p.TenantID).Scan(&dummy); lockErr != nil {
				return fmt.Errorf("lock tenant for refund webhook: %w", lockErr)
			}
			var d *domain.Due
			if p.DueID != uuid.Nil {
				d, err = txDueRepo.GetByIDForUpdate(ctx, p.DueID)
				if err != nil {
					return err
				}
			}
			if lockErr := tx.QueryRow(ctx, `SELECT id FROM payments WHERE id=$1 FOR UPDATE`, p.ID).Scan(&dummy); lockErr != nil {
				return fmt.Errorf("lock payment for refund webhook: %w", lockErr)
			}

			// Forward-only state transition check
			if existingRef != nil && isTerminalRefund(existingRef.Status) && !isTerminalRefund(ref.RefundStatus) {
				return nil // reject regression from terminal status
			}

			source := "system"
			if ref.IsAutoRefund {
				source = "cashfree_auto"
			}
			dbRefundStatus := domain.NormalizeRefundStatus(ref.RefundStatus)
			rfRow := &domain.GatewayRefund{
				PaymentID:       p.ID,
				CFRefundID:      &ref.CFRefundID,
				RefundReference: &ref.RefundID,
				AmountPaise:     ref.RefundAmount,
				Status:          dbRefundStatus,
				Source:          source,
				Reason:          ref.RefundReason,
				RawPayload:      evtRecord.RawPayload,
			}
			if existingRef != nil {
				rfRow.ID = existingRef.ID
				rfRow.PropertyID = existingRef.PropertyID
				rfRow.IdempotencyKey = existingRef.IdempotencyKey
				rfRow.InitiatedBy = existingRef.InitiatedBy
				rfRow.CreatedAt = existingRef.CreatedAt
			}
			if err := txPayRepo.CreateOrUpdateRefund(ctx, rfRow); err != nil {
				return err
			}

			// Financial ledger reversal upon successful refund
			if strings.EqualFold(ref.RefundStatus, "SUCCESS") {
				var mirrorAllocs []domain.RefundAllocationItemPayload

				if !p.IsUnapplied {
					// Step A: Inspect existing refund allocations (pre-created by OwnerRefundPayment)
					type allocItem struct {
						dueID       uuid.UUID
						amountPaise int64
					}
					var existingAllocs []allocItem
					allocRows, qErr := tx.Query(ctx, `SELECT due_id, amount_paise FROM refund_allocations WHERE refund_id = $1`, rfRow.ID)
					if qErr == nil {
						for allocRows.Next() {
							var item allocItem
							if err := allocRows.Scan(&item.dueID, &item.amountPaise); err == nil && item.dueID != uuid.Nil {
								existingAllocs = append(existingAllocs, item)
							}
						}
						allocRows.Close()
					}

					if len(existingAllocs) > 0 {
						// Existing allocations already set by OwnerRefundPayment: preserve them and recompute all touched dues
						for _, item := range existingAllocs {
							targetDue, err := txDueRepo.GetByIDForUpdate(ctx, item.dueID)
							if err == nil && targetDue != nil {
								if err := recomputeDueStatusUnderLock(ctx, txDueRepo, txPayRepo, targetDue); err != nil {
									return err
								}
								mirrorAllocs = append(mirrorAllocs, domain.RefundAllocationItemPayload{
									AmountPaise: item.amountPaise,
									DueKind:     string(targetDue.Kind),
								})
								committedDueIDs = append(committedDueIDs, item.dueID)
							}
						}
					} else {
						// No pre-created allocations (e.g. gateway auto-refund or Cashfree dashboard refund)
						// Fetch payment allocations linked to this payment and allocate LIFO across dues
						pAllocRows, paErr := tx.Query(ctx, `
							SELECT pa.due_id, pa.amount_paise, d.kind
							FROM payment_allocations pa
							JOIN dues d ON d.id = pa.due_id
							WHERE pa.payment_id = $1
							ORDER BY d.due_date DESC, d.id DESC`, p.ID)

						type pAllocItem struct {
							dueID       uuid.UUID
							amountPaise int64
							kind        domain.DueKind
						}
						var pAllocs []pAllocItem
						if paErr == nil {
							for pAllocRows.Next() {
								var pa pAllocItem
								if err := pAllocRows.Scan(&pa.dueID, &pa.amountPaise, &pa.kind); err == nil {
									pAllocs = append(pAllocs, pa)
								}
							}
							pAllocRows.Close()
						}

						if len(pAllocs) > 0 {
							remRefund := ref.RefundAmount
							for _, pa := range pAllocs {
								if remRefund <= 0 {
									break
								}
								prevRefunded, err := txPayRepo.GetPaymentDueRefundedPaise(ctx, p.ID, pa.dueID, rfRow.ID)
								if err != nil {
									return err
								}
								remAlloc := pa.amountPaise - prevRefunded
								if remAlloc <= 0 {
									continue
								}
								netPaid, err := txPayRepo.GetDueNetPaidPaise(ctx, pa.dueID)
								if err != nil {
									return err
								}
								maxAlloc := remAlloc
								if netPaid < maxAlloc {
									maxAlloc = netPaid
								}
								if maxAlloc <= 0 {
									continue
								}
								allocAmt := remRefund
								if allocAmt > maxAlloc {
									allocAmt = maxAlloc
								}
								dueIDCopy := pa.dueID
								if err := txPayRepo.CreateRefundAllocation(ctx, &domain.RefundAllocation{
									RefundID:    rfRow.ID,
									DueID:       &dueIDCopy,
									AmountPaise: allocAmt,
								}); err != nil {
									return err
								}
								targetDue, err := txDueRepo.GetByIDForUpdate(ctx, pa.dueID)
								if err == nil && targetDue != nil {
									if err := recomputeDueStatusUnderLock(ctx, txDueRepo, txPayRepo, targetDue); err != nil {
										return err
									}
									mirrorAllocs = append(mirrorAllocs, domain.RefundAllocationItemPayload{
										AmountPaise: allocAmt,
										DueKind:     string(pa.kind),
									})
									committedDueIDs = append(committedDueIDs, pa.dueID)
								}
								remRefund -= allocAmt
							}
						} else if d != nil {
							// Single due fallback
							prevRefunded, err := txPayRepo.GetPaymentDueRefundedPaise(ctx, p.ID, d.ID, rfRow.ID)
							if err != nil {
								return err
							}
							netPaid, err := txPayRepo.GetDueNetPaidPaise(ctx, d.ID)
							if err != nil {
								return err
							}
							maxAlloc := p.Amount - prevRefunded
							if netPaid < maxAlloc {
								maxAlloc = netPaid
							}
							allocAmt := ref.RefundAmount
							if allocAmt > maxAlloc {
								allocAmt = maxAlloc
							}
							if allocAmt > 0 {
								if err := txPayRepo.CreateRefundAllocation(ctx, &domain.RefundAllocation{
									RefundID:    rfRow.ID,
									DueID:       &d.ID,
									AmountPaise: allocAmt,
								}); err != nil {
									return err
								}
								if err := recomputeDueStatusUnderLock(ctx, txDueRepo, txPayRepo, d); err != nil {
									return err
								}
								mirrorAllocs = append(mirrorAllocs, domain.RefundAllocationItemPayload{
									AmountPaise: allocAmt,
									DueKind:     string(d.Kind),
								})
								committedDueIDs = append(committedDueIDs, d.ID)
							}
						}
					}
				}

				// Enqueue in-transaction refund outbox event (C-05)
				propID := uuid.Nil
				if existingRef != nil && existingRef.PropertyID != uuid.Nil {
					propID = existingRef.PropertyID
				}
				if propID == uuid.Nil && d != nil {
					propID = d.PropertyID
				}
				if propID == uuid.Nil && p.PropertyID != nil {
					propID = *p.PropertyID
				}
				dueKind := string(domain.DueKindRent)
				if len(mirrorAllocs) > 0 {
					dueKind = mirrorAllocs[0].DueKind
				} else if d != nil {
					dueKind = string(d.Kind)
				}
				refundPayload := domain.RefundMirrorPayload{
					PropertyID:  propID,
					RefundID:    rfRow.ID,
					PaymentID:   p.ID,
					AmountPaise: ref.RefundAmount,
					IsUnapplied: p.IsUnapplied,
					Allocations: mirrorAllocs,
					DueKind:     dueKind,
					OccurredAt:  time.Now().UTC(),
				}
				rawPayload, err := json.Marshal(refundPayload)
				if err != nil {
					return fmt.Errorf("marshal refund outbox: %w", err)
				}
				outboxKey := fmt.Sprintf("refund_mirror:%s", rfRow.ID)
				_, err = tx.Exec(ctx, `
					INSERT INTO ledger_outbox_events (event_type, property_id, source_id, payload, idempotency_key, created_at)
					VALUES ($1, $2, $3, $4, $5, NOW())
					ON CONFLICT (idempotency_key) DO NOTHING`,
					domain.LedgerOutboxRefund, propID, rfRow.ID, rawPayload, outboxKey,
				)
				if err != nil {
					return fmt.Errorf("enqueue refund outbox: %w", err)
				}
			}
			return nil
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "refund settle failed"})
			return
		}


		if h.Referrals != nil && len(committedDueIDs) > 0 {
			for _, dueID := range committedDueIDs {
				if dueID != uuid.Nil {
					if err := h.Referrals.ReverseFirstRentPayment(ctx, dueID, p.ID); err != nil {
						slog.Warn("referral reverse on refund webhook failed", "due_id", dueID, "payment_id", p.ID, "err", err)
					}
				}
			}
		}
	} else if h.GatewayPaymentRepo != nil {
		source := "system"
		if ref.IsAutoRefund {
			source = "cashfree_auto"
		}
		dbRefundStatus := domain.NormalizeRefundStatus(ref.RefundStatus)
		rfRow := &domain.GatewayRefund{
			PaymentID:       p.ID,
			CFRefundID:      &ref.CFRefundID,
			RefundReference: &ref.RefundID,
			AmountPaise:     ref.RefundAmount,
			Status:          dbRefundStatus,
			Source:          source,
			Reason:          ref.RefundReason,
			RawPayload:      evtRecord.RawPayload,
		}
		if err := h.GatewayPaymentRepo.CreateOrUpdateRefund(ctx, rfRow); err != nil {
			slog.Error("failed updating refund status from webhook", "err", err, "cf_refund_id", ref.CFRefundID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "update refund failed"})
			return
		}
	}

	if evtRecord.ID != uuid.Nil {
		_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "processed", nil)
	}
	c.Status(http.StatusOK)
}

func (h *Handlers) handleDisputeWebhook(c *gin.Context, disp cashfree.DisputeWebhook, evtRecord *domain.WebhookEvent) {
	ctx := c.Request.Context()
	slog.Default().Error("CASHFREE PAYMENT DISPUTE RECEIVED: OPERATOR ACTION REQUIRED",
		"dispute_id", disp.DisputeID,
		"order_id", disp.OrderID,
		"cf_payment_id", disp.CFPaymentID,
		"dispute_type", disp.DisputeType,
		"dispute_status", disp.DisputeStatus,
		"dispute_amount_paise", disp.DisputeAmount,
		"respond_by", disp.RespondBy,
		"reason_code", disp.ReasonCode,
		"reason_description", disp.ReasonDescription,
	)

	statusMsg := fmt.Sprintf("dispute_id=%s type=%s status=%s respond_by=%s", disp.DisputeID, disp.DisputeType, disp.DisputeStatus, disp.RespondBy)
	if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
		_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "dispute_action_required", &statusMsg)
	}

	if h.IntentStore != nil && disp.OrderID != "" {
		intent, err := h.IntentStore.GetByOrderID(ctx, disp.OrderID)
		if err == nil && intent != nil {
			var propID uuid.UUID
			var tenantID *uuid.UUID
			if h.DueStore != nil {
				if d, err := h.DueStore.GetByID(ctx, intent.DueID); err == nil && d != nil {
					propID = d.PropertyID
					tenantID = &d.TenantID
				}
			}

			payload, _ := json.Marshal(map[string]any{
				"order_id":             disp.OrderID,
				"dispute_id":           disp.DisputeID,
				"dispute_type":         disp.DisputeType,
				"dispute_status":       disp.DisputeStatus,
				"dispute_amount_paise": disp.DisputeAmount,
				"respond_by":           disp.RespondBy,
				"reason_description":   disp.ReasonDescription,
			})
			if h.Events != nil {
				_ = h.Events.Publish(ctx, domain.Event{
					PropertyID: propID,
					TenantID:   tenantID,
					EventType:  domain.EvtPaymentDisputed,
					OccurredAt: time.Now().UTC(),
					Payload:    payload,
				})
			}
			if h.OutboxEvents != nil {
				_ = h.OutboxEvents.InsertEvent(ctx, &domain.OutboxEvent{
					EventType:  string(domain.EvtPaymentDisputed),
					PropertyID: propID,
					TenantID:   tenantID,
					ActorRole:  string(domain.RoleOwner),
					Payload:    payload,
				})
			}
		}
	}

	// Fail-Safe Ledger Invariant: A dispute is a contested claim, NOT an authorized or confirmed refund.
	// We strictly refrain from mutating double-entry journals or resetting dues to pending automatically.
	// Returning HTTP 200 OK acknowledges the webhook delivery to prevent gateway retry storms.
	c.Status(http.StatusOK)
}
