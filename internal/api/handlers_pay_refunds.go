package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/auth"
	"github.com/pg-cashflow/pg-go/internal/cashfree"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"golang.org/x/text/unicode/norm"
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
	ReconcileRefundTransition(ctx context.Context, ref *domain.GatewayRefund, newStatus string) error
}

type postgresRefundTxRunner struct {
	pool *pgxpool.Pool
}

func (r *postgresRefundTxRunner) ReconcileRefundTransition(ctx context.Context, ref *domain.GatewayRefund, newStatus string) error {
	if r.pool == nil {
		return errors.New("database pool unavailable")
	}
	repo := postgres.NewPaymentRepo(r.pool)
	return repo.ReconcileRefundTransition(ctx, ref, newStatus)
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
			existing, lookupErr := txPayRepo.GetRefundByPaymentAndIdempotency(ctx, p.ID, *rfRow.IdempotencyKey)
			if lookupErr == nil && existing != nil {
				if existing.AmountPaise != req.AmountPaise || norm.NFC.String(existing.Reason) != req.Reason {
					return &refundValidationErr{msg: "idempotency key conflict: refund already exists with different amount or reason", status: http.StatusConflict}
				}
				*rfRow = *existing
				return nil
			}
			if lookupErr != nil && !errors.Is(lookupErr, pgx.ErrNoRows) {
				return fmt.Errorf("check refund idempotency under lock: %w", lookupErr)
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
		if req.AmountPaise > int64(paymentAmt)-alreadyRefunded {
			return &refundValidationErr{
				msg:    fmt.Sprintf("refund amount %d paise exceeds remaining balance %d paise", req.AmountPaise, int64(paymentAmt)-alreadyRefunded),
				status: http.StatusBadRequest,
				code:   apierr.CodePaymentRefundExceedsBalance,
			}
		}

		// Calculate LIFO refund allocations across dues (due_date DESC)
		if !isUnapplied {
			if len(duesList) == 0 {
				return &refundValidationErr{
					msg:    "payment has no associated dues for refund allocation",
					status: http.StatusUnprocessableEntity,
					code:   apierr.CodePaymentRefundShortfall,
				}
			}
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
				netPaid, err := txPayRepo.GetDueUnreservedPaidPaise(ctx, d.dueID)
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

			if remRefund > 0 {
				return &refundValidationErr{
					msg:    fmt.Sprintf("refund allocation shortfall: %d paise unallocated across dues", remRefund),
					status: http.StatusUnprocessableEntity,
					code:   apierr.CodePaymentRefundShortfall,
				}
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

		// Enqueue gateway refund dispatch into dedicated payment_commands outbox
		dispatchPayload := domain.RefundDispatchPayload{
			PropertyID: pid,
			RefundID:   rfRow.ID,
			PaymentID:  p.ID,
		}
		rawDispatchPayload, err := json.Marshal(dispatchPayload)
		if err != nil {
			return fmt.Errorf("marshal refund dispatch command: %w", err)
		}
		dispatchKey := fmt.Sprintf("refund_dispatch:%s", rfRow.ID)
		_, err = tx.Exec(ctx, `
			INSERT INTO payment_commands (command_type, property_id, source_id, payload, idempotency_key, status, max_attempts, created_at)
			VALUES ($1, $2, $3, $4, $5, 'pending', $6, NOW())
			ON CONFLICT (idempotency_key) DO NOTHING`,
			domain.PaymentCommandRefundDispatch, pid, rfRow.ID, rawDispatchPayload, dispatchKey, domain.DefaultPaymentCommandMaxAttempts,
		)
		if err != nil {
			return fmt.Errorf("enqueue refund dispatch command: %w", err)
		}

		// Enqueue tenant notification for pending refund into transactional outbox
		if p.TenantID != uuid.Nil {
			notifPayload, mErr := json.Marshal(map[string]any{
				"refund_id":    rfRow.ID.String(),
				"payment_id":   p.ID.String(),
				"amount_paise": rfRow.AmountPaise,
				"reason":       rfRow.Reason,
			})
			if mErr == nil {
				_, _ = tx.Exec(ctx, `
					INSERT INTO outbox_events (event_type, property_id, tenant_id, actor_role, payload, created_at)
					VALUES ($1, $2, $3, $4, $5, NOW())`,
					string(domain.EvtPaymentRefundPending), pid, p.TenantID, string(rfRow.Source), notifPayload,
				)
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
		var curStatus string
		err := tx.QueryRow(ctx, `SELECT status FROM gateway_refunds WHERE id = $1 FOR UPDATE`, rfRow.ID).Scan(&curStatus)
		if err == nil && domain.NormalizeRefundStatus(curStatus) == domain.RefundStatusSucceeded {
			return fmt.Errorf("cannot cleanup refund %s: already succeeded", rfRow.ID)
		}

		txPayRepo := postgres.NewPaymentRepo(tx)
		if err := txPayRepo.CreateOrUpdateRefund(ctx, rfRow); err != nil {
			return fmt.Errorf("mark refund failed: %w", err)
		}
		if domain.NormalizeRefundStatus(rfRow.Status) == domain.RefundStatusSucceeded {
			return fmt.Errorf("cannot cleanup refund %s: status is succeeded", rfRow.ID)
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
				if err != nil {
					return fmt.Errorf("lock due %s for refund recompute: %w", item.dueID, err)
				}
				if d != nil {
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

func (h *Handlers) getRefundLocker() postgres.RefundOperationLocker {
	if h.RefundLocker != nil {
		return h.RefundLocker
	}
	if h.AppPool != nil {
		return postgres.NewRefundOperationLock(h.AppPool)
	}
	if h.Pool != nil {
		return postgres.NewRefundOperationLock(h.Pool)
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
	Reason      string `json:"reason" binding:"required,max=100"`
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
	msg    string
	status int
	code   apierr.Code
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

	_, _ = h.executePaymentRefund(c, pid, uid, paymentID, req, "owner")
}

// GetPaymentRefundHistory handles GET /owner/payments/:id/refunds and GET /manager/payments/:id/refunds.
// It returns the remaining refundable balance, list of historical refunds,
// and any nonterminal pending operation for a payment.
func (h *Handlers) GetPaymentRefundHistory(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	paymentID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}

	if h.GatewayPaymentRepo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payment repository unavailable"})
		return
	}
	if h.TenantStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "tenant access verification unavailable"})
		return
	}
	p, err := h.GatewayPaymentRepo.GetByID(c.Request.Context(), paymentID)
	if err != nil || p == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "payment not found"})
		return
	}

	tenant, err := h.TenantStore.GetByID(c.Request.Context(), p.TenantID)
	if err != nil || tenant == nil || tenant.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "payment not found"})
		return
	}

	refunds, err := h.GatewayPaymentRepo.ListRefundsByPayment(c.Request.Context(), p.ID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed loading refunds"})
		return
	}

	type refundSummary struct {
		ID              uuid.UUID `json:"id"`
		RefundReference *string   `json:"refund_reference,omitempty"`
		AmountPaise     int64     `json:"amount_paise"`
		Status          string    `json:"status"`
		Reason          string    `json:"reason"`
		CreatedAt       time.Time `json:"created_at"`
		UpdatedAt       time.Time `json:"updated_at"`
	}

	list := make([]refundSummary, 0, len(refunds))
	var refundedPaise int64
	var reservedPaise int64
	var pendingOp *refundSummary
	var hasAgg bool

	if aggRepo, ok := h.GatewayPaymentRepo.(interface {
		GetPaymentRefundAggregates(ctx context.Context, paymentID uuid.UUID) (int64, int64, error)
	}); ok {
		rf, res, aErr := aggRepo.GetPaymentRefundAggregates(c.Request.Context(), p.ID)
		if aErr == nil {
			refundedPaise = rf
			reservedPaise = res
			hasAgg = true
		}
	}

	for _, rf := range refunds {
		item := refundSummary{
			ID:              rf.ID,
			RefundReference: rf.RefundReference,
			AmountPaise:     rf.AmountPaise,
			Status:          rf.Status,
			Reason:          rf.Reason,
			CreatedAt:       rf.CreatedAt,
			UpdatedAt:       rf.UpdatedAt,
		}
		list = append(list, item)
		if !hasAgg {
			switch rf.Status {
			case domain.RefundStatusSucceeded:
				refundedPaise += rf.AmountPaise
			case domain.RefundStatusInitiated, domain.RefundStatusPending, domain.RefundStatusOnHold:
				reservedPaise += rf.AmountPaise
			}
		}
		if pendingOp == nil && (rf.Status == domain.RefundStatusInitiated || rf.Status == domain.RefundStatusPending || rf.Status == domain.RefundStatusOnHold) {
			pendingOp = &item
		}
	}

	remainingPaise := int64(p.Amount) - (refundedPaise + reservedPaise)
	if remainingPaise < 0 {
		remainingPaise = 0
	}

	c.JSON(http.StatusOK, gin.H{
		"payment_id":                 p.ID,
		"original_amount_paise":      p.Amount,
		"refunded_amount_paise":      refundedPaise,
		"reserved_amount_paise":      reservedPaise,
		"remaining_refundable_paise": remainingPaise,
		"refunds":                    list,
		"pending_operation":          pendingOp,
	})
}

type OwnerCancelRefundRequest struct {
	Reason string `json:"reason" binding:"required,min=3,max=100"`
	StepUpAuthInput
}

// OwnerCancelRefund handles POST /owner/payments/:id/refunds/:refund_id/cancel.
// Cancels a non-terminal (such as ONHOLD or INITIATED) refund, freeing held reservations.
func (h *Handlers) OwnerCancelRefund(c *gin.Context) {
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
	refundID, ok := ParseUUIDParam(c, "refund_id")
	if !ok {
		return
	}

	var req OwnerCancelRefundRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason required (3-100 characters)"})
		return
	}

	h.executeCancelRefund(c, pid, uid, paymentID, refundID, req)
}

// ManagerCancelRefund handles POST /manager/payments/:id/refunds/:refund_id/cancel.
// It verifies that the property owner has enabled allow_manager_refunds in property settings.
func (h *Handlers) ManagerCancelRefund(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	if _, ok := h.requireManagerRefunds(c, pid); !ok {
		return
	}
	paymentID, ok := ParseUUIDParam(c, "id")
	if !ok {
		return
	}
	refundID, ok := ParseUUIDParam(c, "refund_id")
	if !ok {
		return
	}

	var req OwnerCancelRefundRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason required (3-100 characters)"})
		return
	}

	h.executeCancelRefund(c, pid, uid, paymentID, refundID, req)
}

func (h *Handlers) executeCancelRefund(c *gin.Context, pid, uid, paymentID, refundID uuid.UUID, req OwnerCancelRefundRequest) {
	if h.GatewayPaymentRepo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payment repository unavailable"})
		return
	}

	ctx := c.Request.Context()
	rf, err := h.GatewayPaymentRepo.GetRefundByID(ctx, refundID)
	if err != nil || rf == nil || rf.PaymentID != paymentID || rf.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "refund not found"})
		return
	}

	// Restrict cancellation to owner- or manager-initiated refunds.
	// Dashboard-created and auto-refund records are managed by the gateway directly.
	if rf.Source != "owner" && rf.Source != "manager" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "only owner or manager initiated refunds can be cancelled"})
		return
	}

	if isTerminalRefund(rf.Status) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "refund is already in terminal status"})
		return
	}

	// R4: Mutual exclusion - acquire PostgreSQL session advisory lock for refundID.
	// This prevents concurrent execution against Cashfree with outbox dispatch or poller resubmission.
	locker := h.getRefundLocker()
	if locker != nil {
		acquired, unlock, err := locker.TryLockRefundOperation(ctx, rf.ID)
		if err != nil {
			slog.Error("refund cancel acquire lock error", "error", err, "refund_id", rf.ID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed acquiring refund operation lock"})
			return
		}
		if !acquired {
			apierr.RespondClientErr(c, http.StatusConflict, "refund is currently in flight; please wait before cancelling", apierr.CodePaymentRefundInFlight)
			return
		}
		defer unlock()
	}

	// Require cryptographic step-up verification before cancelling reserved money.
	req.StepUpAuthInput.Purpose = paymentRefundPurpose(paymentID)
	req.StepUpAuthInput.BatchID = nil
	if _, ok := h.verifyDualControlOrStepUp(c, pid, uid, nil, req.StepUpAuthInput); !ok {
		return
	}

	targetRef := ""
	if rf.RefundReference != nil {
		targetRef = *rf.RefundReference
	}

	// If the refund was dispatched to Cashfree (or has an order_id / reference), attempt cancellation at gateway.
	var orderPayload struct {
		OrderID string `json:"order_id"`
	}
	if len(rf.RawPayload) > 0 {
		_ = json.Unmarshal(rf.RawPayload, &orderPayload)
	}

	// Refund cancellation must verify gateway state to prevent releasing reservations while a queued
	// outbox worker might still submit or settle the refund. Missing client or gateway identifiers
	// must fail closed with reservation held.
	if h.CashfreeClient == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "gateway client unavailable; reservation remains held"})
		return
	}
	if orderPayload.OrderID == "" || targetRef == "" {
		slog.Error("refund cancellation missing gateway order_id or reference", "refund_id", rf.ID)
		c.JSON(http.StatusConflict, gin.H{"error": "gateway details missing; reservation remains held"})
		return
	}

	cfResp, cancelErr := h.CashfreeClient.CancelRefund(ctx, orderPayload.OrderID, targetRef, req.Reason)
	if cancelErr != nil {
		var apiErr *cashfree.APIError
		var statusCoder interface{ HTTPStatusCode() int }
		isNotFound := (errors.As(cancelErr, &apiErr) && apiErr.StatusCode == http.StatusNotFound) ||
			(errors.As(cancelErr, &statusCoder) && statusCoder.HTTPStatusCode() == http.StatusNotFound) ||
			strings.Contains(cancelErr.Error(), "status 404") ||
			strings.Contains(cancelErr.Error(), "404 Not Found")

		if isNotFound {
			slog.Warn("gateway returned 404 on refund cancel; provider state unknown; reservation remains held", "refund_id", rf.ID)
			c.JSON(http.StatusConflict, gin.H{"error": "gateway refund state unknown; reservation remains held"})
			return
		}
		slog.Error("gateway cancel refund error", "error", cancelErr, "refund_id", rf.ID)
		c.JSON(http.StatusBadGateway, gin.H{"error": "gateway rejected refund cancellation; reservation remains held"})
		return
	}
	// R5: Validate provider cancellation response against original order, refund reference and amount.
	if err := cashfree.ValidateRefundResponse(orderPayload.OrderID, targetRef, rf.AmountPaise, cfResp); err != nil {
		slog.Warn("gateway returned unverifiable cancellation response", "error", err, "refund_id", rf.ID)
		c.JSON(http.StatusBadGateway, gin.H{"error": "gateway refund cancellation unverifiable; reservation remains held"})
		return
	}
	if domain.NormalizeRefundStatus(cfResp.RefundStatus) != domain.RefundStatusCancelled {
		slog.Warn("gateway returned non-cancelled status on cancel", "status", cfResp.RefundStatus, "refund_id", rf.ID)
		c.JSON(http.StatusBadGateway, gin.H{"error": "gateway refund status is not cancelled; reservation remains held"})
		return
	}

	runner := h.getRefundTxRunner()
	if runner == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database pool unavailable"})
		return
	}

	rf.Status = domain.RefundStatusCancelled
	rf.UpdatedAt = time.Now().UTC()
	if err := runner.CleanupFailedRefund(ctx, rf); err != nil {
		slog.Error("cleanup cancelled refund failed", "error", err, "refund_id", rf.ID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed cancelling refund"})
		return
	}

	// Audit trail: publish refund cancelled event
	if h.Events != nil {
		eventData, _ := json.Marshal(map[string]any{
			"payment_id": paymentID,
			"refund_id":  rf.ID,
			"amount":     rf.AmountPaise,
			"reason":     req.Reason,
			"actor_id":   uid,
		})
		_ = h.Events.Publish(ctx, domain.Event{
			PropertyID: pid,
			EventType:  domain.EvtPaymentRefundCancelled,
			OccurredAt: rf.UpdatedAt,
			Payload:    eventData,
		})
	}

	c.JSON(http.StatusOK, rf)
}

// OwnerResolveReviewRefundRequest represents an operator resolution for an unconfirmed refund in needs_review status.
type OwnerResolveReviewRefundRequest struct {
	Action string `json:"action" binding:"required"` // "cancel" (release reservation) or "confirm_succeeded" (settle refund)
	Reason string `json:"reason" binding:"required"`
	StepUpAuthInput
}

// OwnerResolveReviewRefund handles POST /owner/payments/:id/refunds/:refund_id/resolve-review.
func (h *Handlers) OwnerResolveReviewRefund(c *gin.Context) {
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
	refundID, ok := ParseUUIDParam(c, "refund_id")
	if !ok {
		return
	}
	var req OwnerResolveReviewRefundRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	h.executeResolveReviewRefund(c, pid, uid, paymentID, refundID, req)
}

// ManagerResolveReviewRefund handles POST /manager/payments/:id/refunds/:refund_id/resolve-review.
func (h *Handlers) ManagerResolveReviewRefund(c *gin.Context) {
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
	refundID, ok := ParseUUIDParam(c, "refund_id")
	if !ok {
		return
	}
	var req OwnerResolveReviewRefundRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	h.executeResolveReviewRefund(c, pid, uid, paymentID, refundID, req)
}

func (h *Handlers) executeResolveReviewRefund(c *gin.Context, pid, uid, paymentID, refundID uuid.UUID, req OwnerResolveReviewRefundRequest) {
	if h.GatewayPaymentRepo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payment repository unavailable"})
		return
	}

	ctx := c.Request.Context()
	rf, err := h.GatewayPaymentRepo.GetRefundByID(ctx, refundID)
	if err != nil || rf == nil || rf.PaymentID != paymentID || rf.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "refund not found"})
		return
	}

	// Resolution applies strictly to refunds in 'needs_review'
	if domain.NormalizeRefundStatus(rf.Status) != domain.RefundStatusNeedsReview {
		c.JSON(http.StatusBadRequest, gin.H{"error": "refund is not in needs_review status"})
		return
	}

	cleanAction := strings.ToLower(strings.TrimSpace(req.Action))
	if cleanAction != "cancel" && cleanAction != "confirm_succeeded" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "action must be either 'cancel' or 'confirm_succeeded'"})
		return
	}

	cleanReason := strings.TrimSpace(req.Reason)
	if len(cleanReason) < 3 || len(cleanReason) > 255 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "reason must be between 3 and 255 characters"})
		return
	}

	// R4: Mutual exclusion - acquire PostgreSQL session advisory lock for refundID.
	locker := h.getRefundLocker()
	if locker != nil {
		acquired, unlock, err := locker.TryLockRefundOperation(ctx, rf.ID)
		if err != nil {
			slog.Error("refund resolve review acquire lock error", "error", err, "refund_id", rf.ID)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed acquiring refund operation lock"})
			return
		}
		if !acquired {
			apierr.RespondClientErr(c, http.StatusConflict, "refund is currently in flight; please wait before resolving", apierr.CodePaymentRefundInFlight)
			return
		}
		defer unlock()
	}

	// Require cryptographic step-up verification before resolving uncertain money state.
	req.StepUpAuthInput.Purpose = paymentRefundPurpose(paymentID)
	req.StepUpAuthInput.BatchID = nil
	if _, ok := h.verifyDualControlOrStepUp(c, pid, uid, nil, req.StepUpAuthInput); !ok {
		return
	}

	// Provider Verification: A refund in needs_review must not be resolved from an
	// operator assertion alone. Verify the provider state matches the requested resolution.
	if h.CashfreeClient != nil {
		var orderID string
		var orderPayload struct {
			OrderID string `json:"order_id"`
		}
		if len(rf.RawPayload) > 0 {
			_ = json.Unmarshal(rf.RawPayload, &orderPayload)
			orderID = orderPayload.OrderID
		}
		if orderID == "" && h.GatewayPaymentRepo != nil && h.IntentStore != nil {
			if p, pErr := h.GatewayPaymentRepo.GetByID(ctx, paymentID); pErr == nil && p != nil {
				if p.CFPaymentID != nil && *p.CFPaymentID != "" {
					if intent, err := h.IntentStore.GetByCFPaymentID(ctx, *p.CFPaymentID); err == nil && intent != nil {
						orderID = intent.ProviderOrderID
					}
				}
				if orderID == "" && p.ProviderPaymentID != nil && *p.ProviderPaymentID != "" {
					if intent, err := h.IntentStore.GetByCFPaymentID(ctx, *p.ProviderPaymentID); err == nil && intent != nil {
						orderID = intent.ProviderOrderID
					}
				}
			}
		}
		var targetRef string
		if rf.RefundReference != nil && *rf.RefundReference != "" {
			targetRef = *rf.RefundReference
		} else if rf.IdempotencyKey != nil {
			targetRef = *rf.IdempotencyKey
		}

		if orderID == "" || targetRef == "" {
			slog.Error("refund review resolution missing gateway order_id or reference", "refund_id", rf.ID)
			c.JSON(http.StatusConflict, gin.H{"error": "gateway details missing; cannot verify provider refund status"})
			return
		}

		cfResp, fetchErr := h.CashfreeClient.FetchRefundStatus(ctx, orderID, targetRef)
		if fetchErr != nil {
			slog.Error("gateway fetch refund status error during review resolution", "error", fetchErr, "refund_id", rf.ID)
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed fetching gateway refund status; reservation remains held"})
			return
		}

		if err := cashfree.ValidateRefundResponse(orderID, targetRef, rf.AmountPaise, cfResp); err != nil {
			slog.Error("gateway refund response validation failed during review resolution", "error", err, "refund_id", rf.ID)
			c.JSON(http.StatusBadGateway, gin.H{"error": "gateway refund validation failed: " + err.Error()})
			return
		}

		cfStatus := strings.ToUpper(strings.TrimSpace(cfResp.RefundStatus))
		if cleanAction == "confirm_succeeded" {
			if cfStatus != "SUCCESS" {
				c.JSON(http.StatusConflict, gin.H{
					"error": fmt.Sprintf("gateway refund status is %q; cannot confirm success without provider confirmation", cfResp.RefundStatus),
				})
				return
			}
		} else if cleanAction == "cancel" {
			if cfStatus != "CANCELLED" && cfStatus != "CANCELED" && cfStatus != "FAILED" && cfStatus != "REJECTED" {
				c.JSON(http.StatusConflict, gin.H{
					"error": fmt.Sprintf("gateway refund status is %q; cannot cancel or release reservation while refund is non-terminal at provider", cfResp.RefundStatus),
				})
				return
			}
		}
	}

	runner := h.getRefundTxRunner()
	if runner == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database pool unavailable"})
		return
	}

	targetStatus := domain.RefundStatusCancelled
	if cleanAction == "confirm_succeeded" {
		targetStatus = domain.RefundStatusSucceeded
	}

	if err := runner.ReconcileRefundTransition(ctx, rf, targetStatus); err != nil {
		slog.Error("failed reconciling needs_review refund", "refund_id", rf.ID, "action", cleanAction, "error", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed resolving refund: " + err.Error()})
		return
	}

	// Audit trail: publish resolution event
	if h.Events != nil {
		evtType := domain.EvtPaymentRefundCancelled
		if cleanAction == "confirm_succeeded" {
			evtType = "PaymentRefundResolved"
		}
		eventData, _ := json.Marshal(map[string]any{
			"payment_id": paymentID,
			"refund_id":  rf.ID,
			"amount":     rf.AmountPaise,
			"action":     cleanAction,
			"reason":     cleanReason,
			"actor_id":   uid,
		})
		_ = h.Events.Publish(ctx, domain.Event{
			PropertyID: pid,
			EventType:  evtType,
			OccurredAt: time.Now().UTC(),
			Payload:    eventData,
		})
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "refund resolved successfully",
		"refund":  rf,
	})
}

// ManagerRefundPayment handles POST /manager/payments/:id/refund.
// It verifies that the property owner has enabled allow_manager_refunds in property settings.
func (h *Handlers) ManagerRefundPayment(c *gin.Context) {
	pid, ok := propertyIDFromClaims(c)
	if !ok {
		return
	}
	uid, ok := userIDFromClaims(c)
	if !ok {
		return
	}
	limit, ok := h.requireManagerRefunds(c, pid)
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

	if managerRefundExceedsLimit(limit, req.AmountPaise) {
		apierr.RespondClientErr(c, http.StatusForbidden, "refund amount exceeds the limit set by the property owner; ask the owner to issue it", apierr.CodeRefundManagerLimitExceeded)
		return
	}

	_, _ = h.executePaymentRefund(c, pid, uid, paymentID, req, "manager")
}

// requireManagerRefunds checks the owner toggle and returns the owner-set per-refund limit.
// A nil limit means no limit. It writes a 403 and returns ok=false when the toggle is off.
func (h *Handlers) requireManagerRefunds(c *gin.Context, pid uuid.UUID) (limit *int64, ok bool) {
	if h.OnboardingSettings == nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "manager refunds disabled by property owner"})
		return nil, false
	}
	s, err := h.OnboardingSettings.Get(c.Request.Context(), pid)
	if err != nil || s == nil || !s.AllowManagerRefunds {
		c.JSON(http.StatusForbidden, gin.H{"error": "manager refunds disabled by property owner"})
		return nil, false
	}
	return s.ManagerRefundLimitPaise, true
}

// managerRefundExceedsLimit reports whether amountPaise is above the owner-set limit. A nil limit never blocks.
func managerRefundExceedsLimit(limit *int64, amountPaise int64) bool {
	return limit != nil && amountPaise > *limit
}

var validRefundReasonRegex = regexp.MustCompile(`^[\p{L}\p{M}\p{N}\x{200C}\x{200D} .,_'/-]+$`)

// Bind refund OTPs to the full payment UUID within otp_requests.purpose (32 chars).
// This keeps payout approval codes and codes for other payments out of this flow.
func paymentRefundPurpose(id uuid.UUID) string {
	return "rf" + base64.RawURLEncoding.EncodeToString(id[:])
}

func (h *Handlers) RequestPaymentRefundOTP(c *gin.Context) {
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
	claims, ok := auth.ClaimsFromContext(c)
	if !ok {
		return
	}
	if claims.Role == domain.RoleManager {
		if h.OnboardingSettings == nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "manager refunds disabled by property owner"})
			return
		}
		s, err := h.OnboardingSettings.Get(c.Request.Context(), pid)
		if err != nil || s == nil || !s.AllowManagerRefunds {
			c.JSON(http.StatusForbidden, gin.H{"error": "manager refunds disabled by property owner"})
			return
		}
	}
	if h.GatewayPaymentRepo == nil || h.TenantStore == nil || h.Auth == nil || h.UserStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "refund authorization unavailable"})
		return
	}
	p, err := h.GatewayPaymentRepo.GetByID(c.Request.Context(), paymentID)
	if err != nil || p == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "payment not found"})
		return
	}
	tenant, err := h.TenantStore.GetByID(c.Request.Context(), p.TenantID)
	if err != nil || tenant == nil || tenant.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "payment not found"})
		return
	}
	user, err := h.UserStore.GetByID(c.Request.Context(), uid)
	if err != nil || user == nil || strings.TrimSpace(user.Phone) == "" {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "registered phone required for refund authorization"})
		return
	}
	channel, destination, err := h.Auth.RequestStepUpOTPWithChannel(c.Request.Context(), user.Phone, paymentRefundPurpose(paymentID), nil)
	if err != nil {
		if errors.Is(err, auth.ErrRateLimited) {
			respondErr(c, clientErrWithCode(http.StatusTooManyRequests, "please wait before requesting another code", apierr.CodeAuthRateLimited))
		} else if errors.Is(err, auth.ErrNoDeliveryChannel) {
			respondErr(c, clientErrWithCode(http.StatusUnprocessableEntity, "no delivery channel available", apierr.CodeAuthNoDeliveryChannel))
		} else {
			respondErr(c, err)
		}
		return
	}
	if channel == "sms" {
		destination = maskPhone(destination)
	}
	c.JSON(http.StatusOK, gin.H{"channel": channel, "destination": destination})
}

func (h *Handlers) executePaymentRefund(c *gin.Context, pid, uid, paymentID uuid.UUID, req OwnerRefundRequest, source string) (*domain.GatewayRefund, error) {
	ctx := c.Request.Context()

	if source == "" {
		claims, ok := auth.ClaimsFromContext(c)
		if ok && claims.Role == domain.RoleManager {
			source = "manager"
		} else {
			source = "owner"
		}
	}

	trimmedReason := norm.NFC.String(strings.TrimSpace(req.Reason))
	runeCount := utf8.RuneCountInString(trimmedReason)
	if runeCount < 3 || runeCount > 100 {
		apierr.RespondBindErr(c, "reason must be between 3 and 100 characters", apierr.CodeRefundReasonLength)
		return nil, errors.New("invalid reason length")
	}
	if !validRefundReasonRegex.MatchString(trimmedReason) {
		apierr.RespondBindErr(c, "reason contains invalid characters", apierr.CodeRefundReasonCharacters)
		return nil, errors.New("invalid reason characters")
	}
	req.Reason = trimmedReason

	if h.GatewayPaymentRepo == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "payment repository unavailable"})
		return nil, errors.New("payment repository unavailable")
	}
	if h.CashfreeClient == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "cashfree client unavailable"})
		return nil, errors.New("cashfree client unavailable")
	}
	if h.TenantStore == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "tenant access verification unavailable"})
		return nil, errors.New("tenant access verification unavailable")
	}

	// Unlocked Read 1: Resolve payment
	p, err := h.GatewayPaymentRepo.GetByID(ctx, paymentID)
	if err != nil || p == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "payment not found"})
		return nil, errors.New("payment not found")
	}

	// Verify tenant belongs to property (use uniform 404 message to prevent payment ID enumeration oracle)
	tenant, err := h.TenantStore.GetByID(ctx, p.TenantID)
	if err != nil || tenant == nil || tenant.PropertyID != pid {
		c.JSON(http.StatusNotFound, gin.H{"error": "payment not found"})
		return nil, errors.New("payment not found")
	}

	// Gateway check: Only cashfree provider payments can be refunded via PG API
	if p.Provider != "cashfree" && p.MatchedBy != domain.MatchedByCashfree {
		apierr.RespondClientErr(c, http.StatusBadRequest, "only cashfree gateway payments can be refunded via gateway", apierr.CodePaymentRefundNotEligible)
		return nil, errors.New("invalid provider")
	}

	// Eligibility window check (aligned with Cashfree policy and merchant config)
	now := time.Now().UTC()
	paymentTime := p.CreatedAt
	if !p.MatchedAt.IsZero() {
		paymentTime = p.MatchedAt
	}
	windowDays := h.RefundWindowDays
	if windowDays <= 0 {
		windowDays = 120
	}
	if now.Sub(paymentTime) > time.Duration(windowDays)*24*time.Hour {
		apierr.RespondClientErr(c, http.StatusBadRequest, fmt.Sprintf("payment is older than %d days and cannot be refunded via gateway", windowDays), apierr.CodePaymentRefundNotEligible)
		return nil, fmt.Errorf("older than %d days", windowDays)
	}

	// Idempotency key handling: standard Idempotency-Key with X-Idempotency-Key fallback
	idempotencyKey := strings.TrimSpace(c.GetHeader("Idempotency-Key"))
	if idempotencyKey == "" {
		idempotencyKey = strings.TrimSpace(c.GetHeader("X-Idempotency-Key"))
	}
	if idempotencyKey == "" {
		apierr.RespondClientErr(c, http.StatusBadRequest, "Idempotency-Key header is required for refund operations", apierr.CodeFinanceIdempotencyRequired)
		return nil, errors.New("missing idempotency key")
	}
	if len(idempotencyKey) > 128 {
		apierr.RespondClientErr(c, http.StatusBadRequest, "Idempotency-Key header cannot exceed 128 characters", apierr.CodeRequestInvalidBody)
		return nil, errors.New("idempotency key too long")
	}

	// Unlocked Read 2: Check if already initiated/completed with this idempotency key
	existingRef, lookupErr := h.GatewayPaymentRepo.GetRefundByPaymentAndIdempotency(ctx, p.ID, idempotencyKey)
	if lookupErr == nil && existingRef != nil {
		if existingRef.AmountPaise != req.AmountPaise || norm.NFC.String(existingRef.Reason) != req.Reason {
			c.JSON(http.StatusConflict, gin.H{
				"error": "idempotency key conflict: refund already exists with different amount or reason",
				"code":  "IDEMPOTENCY_CONFLICT",
			})
			return nil, errors.New("idempotency conflict")
		}
		c.JSON(http.StatusOK, existingRef)
		return existingRef, nil
	}
	if lookupErr != nil && !errors.Is(lookupErr, pgx.ErrNoRows) && !errors.Is(lookupErr, domain.ErrNotFound) && lookupErr.Error() != "not found" {
		slog.Error("refund idempotency lookup failed", "payment_id", p.ID, "error", lookupErr)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "refund service temporarily unavailable"})
		return nil, lookupErr
	}

	// Cryptographic step-up / dual-control enforcement. Runs after the idempotent replay
	// lookup above, so a retry of a completed request does not need a fresh one-time code.
	// The client cannot choose an unrelated OTP purpose or payout batch.
	req.StepUpAuthInput.Purpose = paymentRefundPurpose(paymentID)
	req.StepUpAuthInput.BatchID = nil
	if _, ok := h.verifyDualControlOrStepUp(c, pid, uid, nil, req.StepUpAuthInput); !ok {
		return nil, errors.New("step up verification failed")
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
		return nil, errors.New("cannot resolve gateway order ID")
	}

	// Random unique 34-character refund reference: rf<32_char_hex_uuid>
	cleanUUID := strings.ReplaceAll(uuid.New().String(), "-", "")
	refundRef := fmt.Sprintf("rf%s", cleanUUID)

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
		Source:          source,
		InitiatedBy:     &uid,
		Reason:          req.Reason,
		RawPayload:      orderPayload,
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	runner := h.getRefundTxRunner()
	if runner == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "database pool unavailable"})
		return nil, errors.New("database pool unavailable")
	}

	_, err = runner.PrepareRefund(ctx, pid, p, req, rfRow)
	if err != nil {
		var vErr *refundValidationErr
		if errors.As(err, &vErr) {
			status := vErr.status
			if status == 0 {
				status = http.StatusBadRequest
			}
			if vErr.code != "" {
				apierr.RespondClientErr(c, status, vErr.msg, vErr.code)
			} else {
				c.JSON(status, gin.H{"error": vErr.msg})
			}
			return nil, err
		}
		respondErr(c, err)
		return nil, err
	}

	// If this refund row was adopted from a racing or existing request
	if rfRow.ID != originalRefundID {
		if isTerminalRefund(rfRow.Status) || (rfRow.CFRefundID != nil && *rfRow.CFRefundID != "") {
			c.JSON(http.StatusOK, rfRow)
			return rfRow, nil
		}
		// Adopted row in non-terminal state without gateway ID: return 202.
		c.JSON(http.StatusAccepted, gin.H{
			"message": "refund processing or gateway response ambiguous; reconciliation in progress",
			"refund":  rfRow,
		})
		return rfRow, nil
	}

	// Refund is persisted in status "initiated" and enqueued to outbox.
	// Publish domain event for telemetry/consumers.
	if h.Events != nil && p.TenantID != uuid.Nil {
		eventData, _ := json.Marshal(map[string]any{
			"payment_id":   paymentID,
			"refund_id":    rfRow.ID,
			"amount_paise": rfRow.AmountPaise,
			"reason":       rfRow.Reason,
			"actor_id":     uid,
		})
		_ = h.Events.Publish(ctx, domain.Event{
			PropertyID: pid,
			TenantID:   &p.TenantID,
			EventType:  domain.EvtPaymentRefundPending,
			OccurredAt: rfRow.CreatedAt,
			Payload:    eventData,
		})
	}

	// The ledger outbox worker dispatches to Cashfree asynchronously.
	c.JSON(http.StatusAccepted, gin.H{
		"message": "refund initiated; processing via gateway outbox",
		"refund":  rfRow,
	})
	return rfRow, nil
}
