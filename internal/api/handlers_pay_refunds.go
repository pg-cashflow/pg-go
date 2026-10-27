package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type refundAllocItem struct {
	dueID       uuid.UUID
	amountPaise int64
	dueKind     domain.DueKind
}

// RefundTxRunner abstracts the transactional operations of the refund flow for unit testing and production safety.
type RefundTxRunner interface {
	PrepareRefund(ctx context.Context, pid uuid.UUID, p *domain.Payment, req OwnerRefundRequest, rfRow *domain.GatewayRefund) ([]refundAllocItem, error)
	CleanupFailedRefund(ctx context.Context, rfRow *domain.GatewayRefund) error
	SettleRefund(ctx context.Context, pid uuid.UUID, p *domain.Payment, req OwnerRefundRequest, rfRow *domain.GatewayRefund, allocationsToCreate []refundAllocItem) error
}

type postgresRefundTxRunner struct {
	pool *pgxpool.Pool
}

func (r *postgresRefundTxRunner) PrepareRefund(ctx context.Context, pid uuid.UUID, p *domain.Payment, req OwnerRefundRequest, rfRow *domain.GatewayRefund) ([]refundAllocItem, error) {
	if r.pool == nil {
		return nil, errors.New("database pool unavailable")
	}
	var allocationsToCreate []refundAllocItem
	err := postgres.WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
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

		// Locked Idempotency Guard: check if an identical refund was already initiated while waiting for locks
		if rfRow.IdempotencyKey != nil && *rfRow.IdempotencyKey != "" {
			existing, err := txPayRepo.GetRefundByPaymentAndIdempotency(ctx, p.ID, *rfRow.IdempotencyKey)
			if err == nil && existing != nil {
				*rfRow = *existing
				return nil
			}
		}

		// Guard A: Settlement Terminal Boundary Guard
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
				prevRefunded, err := txPayRepo.GetPaymentDueRefundedPaise(ctx, p.ID, d.dueID, rfRow.ID)
				if err != nil {
					return fmt.Errorf("get payment due refunded: %w", err)
				}
				remAlloc := d.allocatedPaise - prevRefunded
				if remAlloc <= 0 {
					continue
				}
				netPaid, err := txPayRepo.GetDueNetPaidPaise(ctx, d.dueID)
				if err != nil {
					return fmt.Errorf("get due net paid: %w", err)
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
		return nil, err
	}
	return allocationsToCreate, nil
}

func (r *postgresRefundTxRunner) CleanupFailedRefund(ctx context.Context, rfRow *domain.GatewayRefund) error {
	if r.pool == nil {
		return errors.New("database pool unavailable")
	}
	return postgres.WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
		txPayRepo := postgres.NewPaymentRepo(tx)
		if err := txPayRepo.CreateOrUpdateRefund(ctx, rfRow); err != nil {
			return fmt.Errorf("mark refund failed: %w", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM refund_allocations WHERE refund_id = $1`, rfRow.ID); err != nil {
			return fmt.Errorf("delete refund allocations: %w", err)
		}
		return nil
	})
}

func (r *postgresRefundTxRunner) SettleRefund(ctx context.Context, pid uuid.UUID, p *domain.Payment, req OwnerRefundRequest, rfRow *domain.GatewayRefund, allocationsToCreate []refundAllocItem) error {
	if r.pool == nil {
		return errors.New("database pool unavailable")
	}
	return postgres.WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
		txPayRepo := postgres.NewPaymentRepo(tx)
		txDueRepo := postgres.NewDueRepo(tx)

		if err := txPayRepo.CreateOrUpdateRefund(ctx, rfRow); err != nil {
			return err
		}

		// If succeeded, recompute dues and enqueue outbox
		switch rfRow.Status {
case "succeeded":
			for _, item := range allocationsToCreate {
				d, err := txDueRepo.GetByIDForUpdate(ctx, item.dueID)
				if err == nil && d != nil {
					if err := recomputeDueStatusUnderLock(ctx, txDueRepo, txPayRepo, d); err != nil {
						return err
					}
				}
			}

			// In-transaction universal refund ledger outbox enqueue (C-05)
			var mirrorAllocs []domain.RefundAllocationItemPayload
			for _, item := range allocationsToCreate {
				mirrorAllocs = append(mirrorAllocs, domain.RefundAllocationItemPayload{
					AmountPaise: item.amountPaise,
					DueKind:     string(item.dueKind),
				})
			}
			refundPayload := domain.RefundMirrorPayload{
				PropertyID:  pid,
				RefundID:    rfRow.ID,
				PaymentID:   p.ID,
				AmountPaise: req.AmountPaise,
				IsUnapplied: p.IsUnapplied,
				Allocations: mirrorAllocs,
				DueKind:     string(domain.DueKindRent),
				OccurredAt:  time.Now().UTC(),
			}
			rawPayload, err := json.Marshal(refundPayload)
			if err != nil {
				return fmt.Errorf("marshal refund mirror outbox: %w", err)
			}
			outboxKey := fmt.Sprintf("refund_mirror:%s", rfRow.ID)
			_, err = tx.Exec(ctx, `
				INSERT INTO ledger_outbox_events (event_type, property_id, source_id, payload, idempotency_key, created_at)
				VALUES ($1, $2, $3, $4, $5, NOW())
				ON CONFLICT (idempotency_key) DO NOTHING`,
				domain.LedgerOutboxRefund, pid, rfRow.ID, rawPayload, outboxKey,
			)
			if err != nil {
				return fmt.Errorf("enqueue refund ledger outbox: %w", err)
			}
		case "failed", "cancelled":
			if _, err := tx.Exec(ctx, `DELETE FROM refund_allocations WHERE refund_id = $1`, rfRow.ID); err != nil {
				return err
			}
		}
		return nil
	})
}

func (h *Handlers) getRefundTxRunner() RefundTxRunner {
	if h.RefundTxRunner != nil {
		return h.RefundTxRunner
	}
	if h.AppPool != nil {
		return &postgresRefundTxRunner{pool: h.AppPool}
	}
	if h.Pool != nil {
		return &postgresRefundTxRunner{pool: h.Pool}
	}
	return nil
}

func isTerminalRefund(status string) bool {
	s := domain.NormalizeRefundStatus(status)
	return s == domain.RefundStatusSucceeded || s == domain.RefundStatusFailed || s == domain.RefundStatusCancelled
}

func RecomputeDueStatusMath(netPaid int64, contractualCeiling int64) (domain.DueStatus, int64) {
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
	Reason      string `json:"reason" binding:"required,min=3,max=100"`
	StepUpAuthInput
}

type dueAllocInfo struct {
	dueID          uuid.UUID
	dueDate        time.Time
	kind           domain.DueKind
	originalAmount int64
	amount         int64
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
	if !p.MatchedAt.IsZero() {
		paymentTime = p.MatchedAt
	}
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
		c.JSON(http.StatusBadRequest, gin.H{"error": "Idempotency-Key header is required for refund operations"})
		return
	}
	if len(idempotencyKey) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Idempotency-Key header cannot exceed 128 characters"})
		return
	}

	// Unlocked Read 2: Check if already initiated/completed with this idempotency key
	existingRef, err := h.GatewayPaymentRepo.GetRefundByPaymentAndIdempotency(ctx, p.ID, idempotencyKey)
	if err == nil && existingRef != nil {
		if existingRef.AmountPaise != req.AmountPaise || existingRef.Reason != req.Reason {
			c.JSON(http.StatusConflict, gin.H{
				"error": "idempotency key conflict: refund already exists with different amount or reason",
				"code":  "IDEMPOTENCY_CONFLICT",
			})
			return
		}
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
	if orderID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot resolve gateway order ID for payment refund"})
		return
	}

	// Deterministic 35-character refund reference: rf_<32_char_hex_uuid>
	cleanUUID := strings.ReplaceAll(uuid.New().String(), "-", "")
	refundRef := fmt.Sprintf("rf_%s", cleanUUID)

	orderPayload, _ := json.Marshal(map[string]any{
		"order_id": orderID,
	})

	originalRefundID := uuid.New()
	rfRow := &domain.GatewayRefund{
		ID:              originalRefundID,
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
		RawPayload:      orderPayload,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	runner := h.getRefundTxRunner()
	if runner == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database pool unavailable"})
		return
	}

	allocationsToCreate, err := runner.PrepareRefund(ctx, pid, p, req, rfRow)
	if err != nil {
		var vErr *refundValidationErr
		if errors.As(err, &vErr) {
			c.JSON(http.StatusBadRequest, gin.H{"error": vErr.msg})
			return
		}
		respondErr(c, err)
		return
	}

	// Always resolve canonical refund reference from rfRow
	targetRefundRef := refundRef
	if rfRow.RefundReference != nil && *rfRow.RefundReference != "" {
		targetRefundRef = *rfRow.RefundReference
	}

	// If this refund row was adopted from a racing or existing request
	if rfRow.ID != originalRefundID {
		if isTerminalRefund(rfRow.Status) || (rfRow.CFRefundID != nil && *rfRow.CFRefundID != "") {
			c.JSON(http.StatusOK, rfRow)
			return
		}
		// Adopted row in non-terminal state without gateway ID: return 202.
		// Do not call gateway again to avoid race and empty allocations.
		c.JSON(http.StatusAccepted, gin.H{
			"message": "refund processing or gateway response ambiguous; reconciliation in progress",
			"refund":  rfRow,
		})
		return
	}

	// Step C: External Gateway Call (ZERO row locks held during external I/O)
	cfIdempotencyKey := idempotencyKey
	if _, err := uuid.Parse(idempotencyKey); err != nil {
		cfIdempotencyKey = uuid.NewSHA1(uuid.NameSpaceURL, []byte(idempotencyKey)).String()
	}
	cfResp, cfErr := h.CashfreeClient.CreateRefund(ctx, orderID, targetRefundRef, req.AmountPaise, req.Reason, cfIdempotencyKey)
	if cfErr != nil {
		slog.Error("cashfree create refund error", "error", cfErr, "payment_id", p.ID, "refund_id", rfRow.ID)

		var apiErr *cashfree.APIError
		isDefinite4xx := errors.As(cfErr, &apiErr) && apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 && apiErr.StatusCode != http.StatusTooManyRequests

		if isDefinite4xx {
			rfRow.Status = "failed"
			// Use context.WithoutCancel so client disconnect does not abort cleanup
			dbCtx := context.WithoutCancel(ctx)
			if cleanupErr := runner.CleanupFailedRefund(dbCtx, rfRow); cleanupErr != nil {
				slog.Error("refund cleanup after gateway failure", "error", cleanupErr, "refund_id", rfRow.ID)
			}
			respondErr(c, clientErr(http.StatusBadGateway, fmt.Sprintf("gateway refund rejected: %s", apiErr.Error())))
			return
		}

		// Ambiguous gateway failure (timeout, dropped connection, 5xx, or 429).
		// Keep refund row in 'initiated' status and retain pre-allocated dues.
		// Return 202 Accepted; the reconciler job or webhook will finalize the status.
		c.JSON(http.StatusAccepted, gin.H{
			"message": "refund processing or gateway response ambiguous; reconciliation in progress",
			"refund":  rfRow,
		})
		return
	}

	// Update status and provider IDs from gateway response
	respStatus := domain.NormalizeRefundStatus(cfResp.RefundStatus)
	rfRow.Status = respStatus
	rfRow.CFRefundID = &cfResp.CFRefundID
	rfRow.ProviderRefundID = &cfResp.CFRefundID

	// Settle or finalize in DB using context.WithoutCancel to survive client disconnect
	dbCtx := context.WithoutCancel(ctx)
	if settleErr := runner.SettleRefund(dbCtx, pid, p, req, rfRow, allocationsToCreate); settleErr != nil {
		slog.Error("refund post-settlement failed", "error", settleErr, "refund_id", rfRow.ID)
		respondErr(c, fmt.Errorf("post-settlement transaction failed: %w", settleErr))
		return
	}

	if rfRow.Status == "succeeded" && h.Referrals != nil {
		for _, item := range allocationsToCreate {
			if err := h.Referrals.ReverseFirstRentPayment(c.Request.Context(), item.dueID, p.ID); err != nil {
				slog.Warn("referral reverse on refund failed", "due_id", item.dueID, "payment_id", p.ID, "err", err)
			}
		}
	}

	c.JSON(http.StatusOK, rfRow)
}
