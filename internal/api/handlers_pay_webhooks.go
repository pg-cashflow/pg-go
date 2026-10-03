package api

import (
	"bytes"
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
	"github.com/pg-cashflow/pg-go/internal/payment"
	"github.com/pg-cashflow/pg-go/internal/postgres"
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

	// Defense-in-depth: when no secret is configured but IntentStore is wired,
	// refuse unauthenticated webhooks. Only skip when gateway is fully absent.
	if h.CashfreeSecret == "" {
		if h.IntentStore != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "payment gateway not configured"})
			return
		}
		c.Status(http.StatusOK)
		return
	}

	sig := c.GetHeader("x-webhook-signature")
	ts := c.GetHeader("x-webhook-timestamp")
	if !cashfree.VerifyWebhookHMAC(h.CashfreeSecret, ts, string(raw), sig) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
		return
	}

	// Verify timestamp tolerance (drift check) if configured
	if h.WebhookToleranceSec > 0 {
		if err := cashfree.VerifyWebhookTimestamp(ts, h.WebhookToleranceSec, time.Now().UTC()); err != nil {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "timestamp tolerance exceeded"})
			return
		}
	}

	// Ingest raw arrival into webhook_events audit log
	evtRecord := &domain.WebhookEvent{
		Provider:         "cashfree",
		Signature:        &sig,
		TimestampHeader:  &ts,
		RawPayload:       raw,
		ProcessingStatus: "received",
	}
	if h.GatewayPaymentRepo != nil {
		_ = h.GatewayPaymentRepo.CreateWebhookEvent(c.Request.Context(), evtRecord)
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
				_ = h.GatewayPaymentRepo.RecordUnmatchedReceipt(ctx, succ.OrderID, succ.CFPaymentID, nil, int64(succ.AmountPaise), "unknown_order", evtRecord.RawPayload)
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
			_ = h.GatewayPaymentRepo.RecordUnmatchedReceipt(ctx, succ.OrderID, succ.CFPaymentID, &intent.ID, int64(succ.AmountPaise), "amount_mismatch", evtRecord.RawPayload)
		}
		c.Status(http.StatusOK)
		return
	}

	// When Pool & GatewayPaymentRepo are wired, run atomic resolve-then-lock settlement:
	if h.Pool != nil && h.GatewayPaymentRepo != nil {
		err := postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
			txPayRepo := postgres.NewPaymentRepo(tx)
			txDueRepo := postgres.NewDueRepo(tx)
			txIntentRepo := postgres.NewPaymentIntentRepo(tx)

			// Step 1: In-transaction deduplication
			firstSeen, err := txPayRepo.RecordProcessedEvent(ctx, "cashfree", "PAYMENT_SETTLED", succ.CFPaymentID, "")
			if err != nil {
				return err
			}
			if !firstSeen {
				return nil // already processed
			}

			// Step 2: Dues snapshot from payment_intent_dues
			snapshot, err := txIntentRepo.GetDuesSnapshot(ctx, intent.ID)
			if err != nil || len(snapshot) == 0 {
				snapshot = []domain.PaymentIntentDue{{DueID: intent.DueID, AmountPaise: int64(intent.AmountPaise)}}
			}

			// Step 3: Resolve first due to determine tenantID
			due0, err := txDueRepo.GetByID(ctx, snapshot[0].DueID)
			if err != nil {
				return err
			}
			tenantID := due0.TenantID

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

			txnID := succ.TxnID()
			cfID := succ.CFPaymentID
			at := time.Now().UTC()

			if allClosed {
				// UNAPPLIED / RACE LOSER BRANCH (e.g. cash paid offline or duplicate payment)
				failureReason := "due_already_settled"
				if lockedIntent.Status == "superseded" {
					failureReason = "session_superseded"
				}
				if err := txPayRepo.RecordUnmatchedReceipt(ctx, succ.OrderID, succ.CFPaymentID, &intent.ID, int64(succ.AmountPaise), failureReason, evtRecord.RawPayload); err != nil {
					return err
				}

				note := "unapplied duplicate: dues already closed, pending owner refund or manual allocation"
				p := &domain.Payment{
					DueID:       snapshot[0].DueID,
					TenantID:    tenantID,
					UPITxnID:    &txnID,
					CFPaymentID: &cfID,
					Amount:      succ.AmountPaise,
					MatchedBy:   domain.MatchedByCashfree,
					MatchedAt:   at,
					IsUnapplied: true,
					RawNote:     &note,
				}
				if err := txPayRepo.Create(ctx, p); err != nil {
					return err
				}
				if h.Finance != nil {
					if mirrorErr := h.Finance.MirrorUnappliedPayment(ctx, due0.PropertyID, p.ID, int64(succ.AmountPaise), at); mirrorErr != nil {
						// Ledger-gap: payment row is committed but the journal entry failed.
						// The C-2 deferred trigger will reject a future unbalanced commit for this
						// source_id. Log at ERROR for operator alerting; do NOT roll back the
						// payment to avoid re-crediting the customer.
						slog.Default().Error("LEDGER GAP: MirrorUnappliedPayment failed after payment committed",
							"payment_id", p.ID,
							"property_id", due0.PropertyID,
							"amount_paise", succ.AmountPaise,
							"err", mirrorErr,
						)
					}
				}
				if err := txIntentRepo.MarkPaid(ctx, intent.ID, cfID); err != nil {
					return err
				}
				return nil
			}

			// APPLIED TO OPEN DUES BRANCH
			p := &domain.Payment{
				DueID:       snapshot[0].DueID,
				TenantID:    tenantID,
				UPITxnID:    &txnID,
				CFPaymentID: &cfID,
				Amount:      succ.AmountPaise,
				MatchedBy:   domain.MatchedByCashfree,
				MatchedAt:   at,
				IsUnapplied: false,
			}
			if err := txPayRepo.Create(ctx, p); err != nil {
				return err
			}

			remainingPaise := int64(succ.AmountPaise)
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
				credit := targetDue.ApplyPayment(int(allocPaise), at)
				if err := txDueRepo.Update(ctx, targetDue); err != nil {
					return err
				}
				if credit > 0 {
					if _, err := tx.Exec(ctx, `UPDATE tenants SET credit_balance_paise = credit_balance_paise + $2 WHERE id=$1`, tenantID, credit); err != nil {
						return err
					}
				}
				remainingPaise -= allocPaise
			}

			if h.Finance != nil {
				if mirrorErr := h.Finance.MirrorPayment(ctx, p, due0); mirrorErr != nil {
					// Ledger-gap: payment row is committed but the journal entry failed.
					// The C-2 deferred trigger will reject a future unbalanced commit for this
					// source_id. Log at ERROR for operator alerting; do NOT roll back the
					// payment to avoid re-crediting the customer.
					slog.Default().Error("LEDGER GAP: MirrorPayment failed after payment committed",
						"payment_id", p.ID,
						"due_id", due0.ID,
						"property_id", due0.PropertyID,
						"err", mirrorErr,
					)
				}
			}
			if err := txIntentRepo.MarkPaid(ctx, intent.ID, cfID); err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "settle failed"})
			return
		}
		if evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "processed", nil)
		}
		c.Status(http.StatusOK)
		return
	}

	// Legacy fallback path for test stubs without a pgxpool
	var dedupKey string
	if succ.CFPaymentID != "" {
		dedupKey = fmt.Sprintf("cashfree:pg:%s", succ.CFPaymentID)
		if existing, err := h.IntentStore.GetByCFPaymentID(ctx, succ.CFPaymentID); err == nil && existing != nil {
			c.Status(http.StatusOK)
			return
		}
	}
	txn := succ.TxnID()
	if dedupKey != "" {
		_, err = h.Payments.GatewaySettle(ctx, intent.DueID, succ.AmountPaise, txn, dedupKey)
	} else {
		_, err = h.Payments.GatewaySettle(ctx, intent.DueID, succ.AmountPaise, txn)
	}
	if err != nil {
		if errors.Is(err, payment.ErrDuplicateTxn) || errors.Is(err, payment.ErrDueNotOpen) || errors.Is(err, payment.ErrEmptyTxnID) {
			c.Status(http.StatusOK)
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "settle failed"})
		return
	}
	if succ.CFPaymentID != "" {
		_ = h.IntentStore.MarkPaid(ctx, intent.ID, succ.CFPaymentID)
	}
	if h.GatewayPaymentRepo != nil && evtRecord.ID != uuid.Nil {
		_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "processed", nil)
	}
	c.Status(http.StatusOK)
}

func (h *Handlers) handlePaymentFailedWebhook(c *gin.Context, fail cashfree.FailedWebhook, evtRecord *domain.WebhookEvent) {
	ctx := c.Request.Context()
	if h.Pool != nil && h.GatewayPaymentRepo != nil {
		_ = postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
			txPayRepo := postgres.NewPaymentRepo(tx)
			firstSeen, err := txPayRepo.RecordProcessedEvent(ctx, "cashfree", "PAYMENT_FAILED_WEBHOOK", fail.CFPaymentID, "")
			if err != nil || !firstSeen {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE payment_intents SET status='failed', updated_at=NOW() WHERE provider_order_id=$1`, fail.OrderID); err != nil {
				return err
			}
			return nil
		})
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
	existingRef, _ := h.GatewayPaymentRepo.GetRefundByCFRefundID(ctx, ref.CFRefundID)
	var p *domain.Payment
	if existingRef != nil {
		p, _ = h.GatewayPaymentRepo.GetByID(ctx, existingRef.PaymentID)
	} else if ref.CFPaymentID != "" {
		p, _ = h.GatewayPaymentRepo.GetByCFPaymentID(ctx, ref.CFPaymentID)
	}

	if p == nil {
		_ = h.GatewayPaymentRepo.RecordUnmatchedReceipt(ctx, ref.OrderID, ref.CFPaymentID, nil, ref.RefundAmount, "payment_not_found", evtRecord.RawPayload)
		if evtRecord.ID != uuid.Nil {
			_ = h.GatewayPaymentRepo.UpdateWebhookEventStatus(ctx, evtRecord.ID, "unmatched", nil)
		}
		c.Status(http.StatusOK)
		return
	}

	if h.DueStore != nil {
		_, err := h.DueStore.GetByID(ctx, p.DueID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "due lookup failed"})
			return
		}
	}

	// Step 2: Lock hierarchy and forward-only status transition
	if h.Pool != nil {
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
			var dummy uuid.UUID
			_ = tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id=$1 FOR UPDATE`, p.TenantID).Scan(&dummy)
			d, err := txDueRepo.GetByIDForUpdate(ctx, p.DueID)
			if err != nil {
				return err
			}
			_ = tx.QueryRow(ctx, `SELECT id FROM payments WHERE id=$1 FOR UPDATE`, p.ID).Scan(&dummy)

			// Forward-only state transition check
			if existingRef != nil && isTerminalRefund(existingRef.Status) && !isTerminalRefund(ref.RefundStatus) {
				return nil // reject regression from terminal status
			}

			source := "system"
			if ref.IsAutoRefund {
				source = "cashfree_auto"
			}
			rfRow := &domain.GatewayRefund{
				PaymentID:       p.ID,
				CFRefundID:      &ref.CFRefundID,
				RefundReference: &ref.RefundID,
				AmountPaise:     ref.RefundAmount,
				Status:          strings.ToLower(ref.RefundStatus),
				Source:          source,
				Reason:          ref.RefundReason,
				RawPayload:      evtRecord.RawPayload,
			}
			if err := txPayRepo.CreateOrUpdateRefund(ctx, rfRow); err != nil {
				return err
			}

			// Financial ledger reversal upon successful refund
			if strings.EqualFold(ref.RefundStatus, "SUCCESS") {
				now := time.Now().UTC()
				if p.IsUnapplied {
					if h.Finance != nil {
						if mirrorErr := h.Finance.MirrorRefund(ctx, d.PropertyID, rfRow.ID, ref.RefundAmount, true, d.Kind, now); mirrorErr != nil {
							// Ledger-gap: refund row is committed but the reversal journal failed.
							// Log at ERROR for operator alerting; do NOT roll back the refund record.
							slog.Default().Error("LEDGER GAP: MirrorRefund (unapplied) failed after refund committed",
								"refund_id", rfRow.ID,
								"property_id", d.PropertyID,
								"amount_paise", ref.RefundAmount,
								"err", mirrorErr,
							)
						}
					}
				} else {
					if err := txPayRepo.CreateRefundAllocation(ctx, &domain.RefundAllocation{
						RefundID:    rfRow.ID,
						DueID:       &d.ID,
						AmountPaise: ref.RefundAmount,
					}); err != nil {
						return err
					}
					if err := recomputeDueStatusUnderLock(ctx, txDueRepo, txPayRepo, d); err != nil {
						return err
					}
					if h.Finance != nil {
						if mirrorErr := h.Finance.MirrorRefund(ctx, d.PropertyID, rfRow.ID, ref.RefundAmount, false, d.Kind, now); mirrorErr != nil {
							// Ledger-gap: refund row is committed but the reversal journal failed.
							// Log at ERROR for operator alerting; do NOT roll back the refund record.
							slog.Default().Error("LEDGER GAP: MirrorRefund (applied) failed after refund committed",
								"refund_id", rfRow.ID,
								"property_id", d.PropertyID,
								"amount_paise", ref.RefundAmount,
								"err", mirrorErr,
							)
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "refund settle failed"})
			return
		}
	} else if h.GatewayPaymentRepo != nil {
		source := "system"
		if ref.IsAutoRefund {
			source = "cashfree_auto"
		}
		rfRow := &domain.GatewayRefund{
			PaymentID:       p.ID,
			CFRefundID:      &ref.CFRefundID,
			RefundReference: &ref.RefundID,
			AmountPaise:     ref.RefundAmount,
			Status:          strings.ToUpper(strings.TrimSpace(ref.RefundStatus)),
			Source:          source,
			Reason:          ref.RefundReason,
			RawPayload:      evtRecord.RawPayload,
		}
		_ = h.GatewayPaymentRepo.CreateOrUpdateRefund(ctx, rfRow)
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

