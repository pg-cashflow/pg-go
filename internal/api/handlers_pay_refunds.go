package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

func isTerminalRefund(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	return s == "success" || s == "succeeded" || s == "failed" || s == "cancelled"
}

func RecomputeDueStatusMath(netPaid int64, contractualCeiling int) (domain.DueStatus, int) {
	return domain.RecomputeDueStatusMath(netPaid, contractualCeiling)
}

func recomputeDueStatusUnderLock(ctx context.Context, txDueRepo *postgres.DueRepo, txPayRepo *postgres.PaymentRepo, d *domain.Due) error {
	netPaid, err := txPayRepo.GetDueNetPaidPaise(ctx, d.ID)
	if err != nil {
		return err
	}
	// Contractual ceiling: If due was prorated at departure, ContractualCeilingPaise survives
	// even when d.Amount drops to 0 on full settlement.
	ceiling := d.OriginalAmount
	if d.ContractualCeilingPaise != nil && *d.ContractualCeilingPaise > 0 {
		ceiling = *d.ContractualCeilingPaise
	}

	newStatus, newAmount := RecomputeDueStatusMath(netPaid, ceiling)
	d.Status = newStatus
	d.Amount = newAmount
	if d.Status != domain.DueStatusPaid {
		d.PaidAt = nil
	}
	return txDueRepo.Update(ctx, d)
}

type OwnerRefundRequest struct {
	AmountPaise int64  `json:"amount_paise" binding:"required,gt=0"`
	Reason      string `json:"reason" binding:"required"`
	StepUpAuthInput
}

type dueAllocInfo struct {
	dueID          uuid.UUID
	dueDate        time.Time
	kind           domain.DueKind
	originalAmount int
	amount         int
	status         domain.DueStatus
	allocatedPaise int64
}

type refundValidationErr struct {
	msg string
}

func (e *refundValidationErr) Error() string { return e.msg }

// OwnerRefundPayment handles POST /owner/payments/:id/refund.
func (h *Handlers) OwnerRefundPayment(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	paymentID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	var req OwnerRefundRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: amount_paise must be positive and reason required"})
		return
	}
	ctx := c.Request.Context()

	if h.GatewayPaymentRepo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payment repository unavailable"})
		return
	}
	if h.CashfreeClient == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cashfree client unavailable"})
		return
	}

	// Unlocked Read 1: Resolve payment
	p, err := h.GatewayPaymentRepo.GetByID(ctx, paymentID)
	if err != nil || p == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "payment not found"})
		return
	}

	// Verify tenant belongs to property
	tenant, err := h.TenantStore.GetByID(ctx, p.TenantID)
	if err != nil || tenant == nil || tenant.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "payment not found for property"})
		return
	}

	// Cryptographic step-up / dual-control enforcement
	if _, ok := h.verifyDualControlOrStepUp(c, pid, uid, nil, req.StepUpAuthInput); !ok {
		return
	}

	// Gateway check: Only cashfree provider payments can be refunded via PG API
	if p.Provider != "cashfree" && p.MatchedBy != domain.MatchedByCashfree {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only cashfree gateway payments can be refunded via gateway"})
		return
	}

	// 180-day eligibility window check
	now := time.Now().UTC()
	paymentTime := p.CreatedAt
	if now.Sub(paymentTime) > 180*24*time.Hour {
		c.JSON(http.StatusBadRequest, gin.H{"error": "payment is older than 180 days and cannot be refunded via gateway"})
		return
	}

	// Idempotency key handling: standard Idempotency-Key with X-Idempotency-Key fallback
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(c.GetHeader("X-Idempotency-Key"))
	}
	if idempotencyKey == "" {
		idempotencyKey = uuid.New().String()
	}

	// Unlocked Read 2: Check if already initiated/completed with this idempotency key
	existingRef, err := h.GatewayPaymentRepo.GetRefundByPaymentAndIdempotency(ctx, p.ID, idempotencyKey)
	if err == nil && existingRef != nil {
		c.JSON(http.StatusOK, existingRef)
		return
	}

	// Resolve Cashfree Order ID
	var orderID string
	if p.CFPaymentID != nil && *p.CFPaymentID != "" && h.IntentStore != nil {
		if intent, err := h.IntentStore.GetByCFPaymentID(ctx, *p.CFPaymentID); err == nil && intent != nil {
			orderID = intent.ProviderOrderID
		}
	}
	if orderID == "" && p.ProviderPaymentID != nil && *p.ProviderPaymentID != "" && h.IntentStore != nil {
		if intent, err := h.IntentStore.GetByCFPaymentID(ctx, *p.ProviderPaymentID); err == nil && intent != nil {
			orderID = intent.ProviderOrderID
		}
	}
	if orderID == "" && p.DueID != uuid.Nil && h.DueStore != nil {
		if d, err := h.DueStore.GetByID(ctx, p.DueID); err == nil && d != nil {
			orderID = fmt.Sprintf("order_%s", d.DueCode)
		}
	}
	if orderID == "" {
		orderID = fmt.Sprintf("order_%s", p.ID.String()[:8])
	}

	// Deterministic 35-character refund reference: rf_<32_char_hex_uuid>
	cleanUUID := strings.ReplaceAll(uuid.New().String(), "-", "")
	refundRef := fmt.Sprintf("rf_%s", cleanUUID)

	rfRow := &domain.GatewayRefund{
		ID:              uuid.New(),
		PaymentID:       p.ID,
		PropertyID:      pid,
		Provider:        "cashfree",
		RefundReference: &refundRef,
		IdempotencyKey:  &idempotencyKey,
		AmountPaise:     req.AmountPaise,
		Status:          "initiated",
		Source:          "owner",
		InitiatedBy:     &uid,
		Reason:          req.Reason,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	type refundAllocItem struct {
		dueID       uuid.UUID
		amountPaise int64
		dueKind     domain.DueKind
	}
	var allocationsToCreate []refundAllocItem

	// Step B: Atomic Row Lock & Allocation Preparation (Universal Lock Hierarchy)
	if h.Pool != nil {
		err := postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
			txPayRepo := postgres.NewPaymentRepo(tx)

			// 1. Lock Tenant (1)
			var dummy uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id=$1 FOR UPDATE`, p.TenantID).Scan(&dummy); err != nil {
				return fmt.Errorf("lock tenant: %w", err)
			}

			// Gather dues linked to this payment via payment_allocations
			rows, err := tx.Query(ctx, `
				SELECT pa.due_id, pa.amount_paise, d.due_date, d.kind, d.original_amount, d.amount, d.status
				FROM payment_allocations pa
				JOIN dues d ON d.id = pa.due_id
				WHERE pa.payment_id = $1
				ORDER BY d.due_date ASC, d.id ASC`, p.ID)
			if err != nil {
				return fmt.Errorf("query payment allocations: %w", err)
			}
			defer rows.Close()

			var duesList []dueAllocInfo
			for rows.Next() {
				var info dueAllocInfo
				if err := rows.Scan(&info.dueID, &info.allocatedPaise, &info.dueDate, &info.kind, &info.originalAmount, &info.amount, &info.status); err != nil {
					return err
				}
				duesList = append(duesList, info)
			}
			rows.Close()

			// 3. Lock Dues (3) in topological sort order: ORDER BY due_date ASC, id ASC
			if len(duesList) > 0 {
				dueIDs := make([]uuid.UUID, len(duesList))
				for i, d := range duesList {
					dueIDs[i] = d.dueID
				}
				_, err = tx.Exec(ctx, `
					SELECT id FROM dues 
					WHERE id = ANY($1) 
					ORDER BY due_date ASC, id ASC FOR UPDATE`, dueIDs)
				if err != nil {
					return fmt.Errorf("lock dues: %w", err)
				}
			}

			// 6. Lock Payment (6)
			var paymentAmt int
			var isUnapplied bool
			if err := tx.QueryRow(ctx, `
				SELECT amount, is_unapplied 
				FROM payments 
				WHERE id = $1 FOR UPDATE`, p.ID).Scan(&paymentAmt, &isUnapplied); err != nil {
				return fmt.Errorf("lock payment: %w", err)
			}

			// 7. Lock Payment Allocations (7)
			_, err = tx.Exec(ctx, `
				SELECT id FROM payment_allocations 
				WHERE payment_id = $1 
				ORDER BY due_id ASC, id ASC FOR UPDATE`, p.ID)
			if err != nil {
				return fmt.Errorf("lock payment allocations: %w", err)
			}

			// 8. Lock Gateway Refunds (8)
			_, err = tx.Exec(ctx, `
				SELECT id FROM gateway_refunds 
				WHERE payment_id = $1 
				ORDER BY created_at ASC, id ASC FOR UPDATE`, p.ID)
			if err != nil {
				return fmt.Errorf("lock gateway refunds: %w", err)
			}

			// Guard A: Settlement Terminal Boundary Guard
			// Reject refunds if the tenant's departure is already approved/refunded,
			// or if any linked dues have departure_due_adjustments.
			var settledDepartureExists bool
			err = tx.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM tenant_departures 
					WHERE tenant_id = $1 AND status IN ('approved', 'refunded')
				)`, p.TenantID).Scan(&settledDepartureExists)
			if err != nil {
				return fmt.Errorf("check settled departure: %w", err)
			}
			if settledDepartureExists {
				return &refundValidationErr{msg: "Cannot issue gateway refund against payment tied to a settled departure. Deposit and rent adjustments must be resolved via departure disbursement/payout ledger."}
			}

			checkDueIDs := make([]uuid.UUID, 0, len(duesList)+1)
			for _, d := range duesList {
				checkDueIDs = append(checkDueIDs, d.dueID)
			}
			if p.DueID != uuid.Nil {
				checkDueIDs = append(checkDueIDs, p.DueID)
			}
			if len(checkDueIDs) > 0 {
				var adjExists bool
				err = tx.QueryRow(ctx, `
					SELECT EXISTS (
						SELECT 1 FROM departure_due_adjustments 
						WHERE due_id = ANY($1)
					)`, checkDueIDs).Scan(&adjExists)
				if err != nil {
					return fmt.Errorf("check departure due adjustments: %w", err)
				}
				if adjExists {
					return &refundValidationErr{msg: "Cannot issue gateway refund against payment tied to a settled departure. Deposit and rent adjustments must be resolved via departure disbursement/payout ledger."}
				}
			}

			// Validate remaining refundable balance under lock
			alreadyRefunded, err := txPayRepo.GetPaymentRefundedPaise(ctx, p.ID)
			if err != nil {
				return fmt.Errorf("get refunded paise: %w", err)
			}
			if alreadyRefunded+req.AmountPaise > int64(paymentAmt) {
				return &refundValidationErr{msg: fmt.Sprintf("refund amount %d paise exceeds remaining balance %d paise", req.AmountPaise, int64(paymentAmt)-alreadyRefunded)}
			}

			// Calculate LIFO refund allocations across dues (due_date DESC)
			if !isUnapplied && len(duesList) > 0 {
				remRefund := req.AmountPaise
				for i := len(duesList) - 1; i >= 0 && remRefund > 0; i-- {
					d := duesList[i]
					netPaid, err := txPayRepo.GetDueNetPaidPaise(ctx, d.dueID)
					if err != nil {
						return fmt.Errorf("get due net paid: %w", err)
					}
					maxAlloc := d.allocatedPaise
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
					allocationsToCreate = append(allocationsToCreate, refundAllocItem{
						dueID:       d.dueID,
						amountPaise: allocAmt,
						dueKind:     d.kind,
					})
					remRefund -= allocAmt
				}
			}

			// Pre-insert gateway_refunds in status 'initiated'
			if err := txPayRepo.CreateOrUpdateRefund(ctx, rfRow); err != nil {
				return fmt.Errorf("create initiated refund: %w", err)
			}

			// Pre-insert refund_allocations
			for _, item := range allocationsToCreate {
				dueIDCopy := item.dueID
				if err := txPayRepo.CreateRefundAllocation(ctx, &domain.RefundAllocation{
					RefundID:    rfRow.ID,
					DueID:       &dueIDCopy,
					AmountPaise: item.amountPaise,
				}); err != nil {
					return fmt.Errorf("create refund allocation: %w", err)
				}
			}

			return nil
		})
		if err != nil {
			var vErr *refundValidationErr
			if errors.As(err, &vErr) {
				c.JSON(http.StatusBadRequest, gin.H{"error": vErr.msg})
				return
			}
			respondErr(c, err)
			return
		}
	} else {
		// Mock fallback when Pool is nil
		alreadyRefunded, _ := h.GatewayPaymentRepo.GetPaymentRefundedPaise(ctx, p.ID)
		if alreadyRefunded+req.AmountPaise > int64(p.Amount) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "refund amount exceeds remaining balance"})
			return
		}
		_ = h.GatewayPaymentRepo.CreateOrUpdateRefund(ctx, rfRow)
	}

	// Step C: External Gateway Call (ZERO row locks held during external I/O)
	cfResp, cfErr := h.CashfreeClient.CreateRefund(ctx, orderID, refundRef, req.AmountPaise, req.Reason, idempotencyKey)
	if cfErr != nil {
		slog.Error("cashfree create refund error", "error", cfErr, "payment_id", p.ID, "refund_id", rfRow.ID)
		rfRow.Status = "failed"
		if h.Pool != nil {
			_ = postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
				txPayRepo := postgres.NewPaymentRepo(tx)
				_ = txPayRepo.CreateOrUpdateRefund(ctx, rfRow)
				_, _ = tx.Exec(ctx, `DELETE FROM refund_allocations WHERE refund_id = $1`, rfRow.ID)
				return nil
			})
		} else {
			_ = h.GatewayPaymentRepo.CreateOrUpdateRefund(ctx, rfRow)
		}
		respondErr(c, clientErr(http.StatusBadGateway, "gateway refund failed"))
		return
	}

	// Update status and provider IDs from gateway response
	respStatus := strings.ToLower(strings.TrimSpace(cfResp.RefundStatus))
	if respStatus == "success" {
		respStatus = "succeeded"
	}
	rfRow.Status = respStatus
	rfRow.CFRefundID = &cfResp.CFRefundID
	rfRow.ProviderRefundID = &cfResp.CFRefundID

	// Settle or finalize in DB
	if h.Pool != nil {
		err := postgres.WithinTx(ctx, h.Pool, func(tx pgx.Tx) error {
			txPayRepo := postgres.NewPaymentRepo(tx)
			txDueRepo := postgres.NewDueRepo(tx)

			if err := txPayRepo.CreateOrUpdateRefund(ctx, rfRow); err != nil {
				return err
			}

			// If succeeded, recompute dues
			if rfRow.Status == "succeeded" {
				for _, item := range allocationsToCreate {
					d, err := txDueRepo.GetByIDForUpdate(ctx, item.dueID)
					if err == nil && d != nil {
						if err := recomputeDueStatusUnderLock(ctx, txDueRepo, txPayRepo, d); err != nil {
							return err
						}
					}
				}
			} else if rfRow.Status == "failed" || rfRow.Status == "cancelled" {
				if _, err := tx.Exec(ctx, `DELETE FROM refund_allocations WHERE refund_id = $1`, rfRow.ID); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			slog.Error("refund post-settlement failed", "error", err, "refund_id", rfRow.ID)
		}
	} else {
		_ = h.GatewayPaymentRepo.CreateOrUpdateRefund(ctx, rfRow)
	}

	// Post financial ledger reversal outside row locks
	if rfRow.Status == "succeeded" && h.Finance != nil {
		finTime := time.Now().UTC()
		if p.IsUnapplied {
			if mirrorErr := h.Finance.MirrorRefund(ctx, pid, rfRow.ID, req.AmountPaise, true, domain.DueKindRent, finTime); mirrorErr != nil {
				// Ledger-gap: refund row is committed but the reversal journal failed.
				// Log at ERROR for operator alerting; the refund record is authoritative.
				slog.Error("LEDGER GAP: MirrorRefund (unapplied) failed after manual refund committed",
					"refund_id", rfRow.ID,
					"property_id", pid,
					"amount_paise", req.AmountPaise,
					"err", mirrorErr,
				)
			}
		} else {
			for _, item := range allocationsToCreate {
				if mirrorErr := h.Finance.MirrorRefund(ctx, pid, rfRow.ID, item.amountPaise, false, item.dueKind, finTime); mirrorErr != nil {
					// Ledger-gap: refund row is committed but the reversal journal failed.
					// Log at ERROR for operator alerting; the refund record is authoritative.
					slog.Error("LEDGER GAP: MirrorRefund (applied) failed after manual refund committed",
						"refund_id", rfRow.ID,
						"property_id", pid,
						"amount_paise", item.amountPaise,
						"err", mirrorErr,
					)
				}
			}
		}
	}

	c.JSON(http.StatusOK, rfRow)
}

