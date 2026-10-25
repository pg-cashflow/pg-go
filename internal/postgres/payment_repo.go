package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type PaymentRepo struct{ db DBTX }

func NewPaymentRepo(db DBTX) *PaymentRepo { return &PaymentRepo{db: db} }

func (r *PaymentRepo) WithTx(tx pgx.Tx) *PaymentRepo { return &PaymentRepo{db: tx} }

const paymentCols = `id, due_id, tenant_id, upi_txn_id, cf_payment_id, provider_payment_id, provider, amount, matched_by, recorded_by, matched_at, raw_note, is_unapplied, created_at, property_id`

func scanPayment(row pgx.Row) (*domain.Payment, error) {
	var p domain.Payment
	var dueID *uuid.UUID
	err := row.Scan(&p.ID, &dueID, &p.TenantID, &p.UPITxnID, &p.CFPaymentID, &p.ProviderPaymentID, &p.Provider, &p.Amount, &p.MatchedBy, &p.RecordedBy, &p.MatchedAt, &p.RawNote, &p.IsUnapplied, &p.CreatedAt, &p.PropertyID)
	if err != nil {
		return nil, err
	}
	if dueID != nil {
		p.DueID = *dueID
	}
	if p.ProviderPaymentID == nil && p.CFPaymentID != nil {
		p.ProviderPaymentID = p.CFPaymentID
	}
	if p.CFPaymentID == nil && p.ProviderPaymentID != nil {
		p.CFPaymentID = p.ProviderPaymentID
	}
	return &p, nil
}

func (r *PaymentRepo) Create(ctx context.Context, p *domain.Payment) error {
	insertAll := func(repo *PaymentRepo) error {
		now := time.Now().UTC()
		if p.MatchedAt.IsZero() {
			p.MatchedAt = now
		}
		p.CreatedAt = now
		if p.Provider == "" {
			switch p.MatchedBy {
case domain.MatchedByCash:
				p.Provider = "cash"
			case domain.MatchedByManual:
				p.Provider = "manual"
			default:
				p.Provider = "cashfree"
			}
		}
		if p.Provider == "cashfree" {
			if p.CFPaymentID == nil && p.ProviderPaymentID != nil {
				p.CFPaymentID = p.ProviderPaymentID
			}
			if p.ProviderPaymentID == nil && p.CFPaymentID != nil {
				p.ProviderPaymentID = p.CFPaymentID
			}
		}
		var dueIDArg *uuid.UUID
		if p.DueID != uuid.Nil {
			dueIDArg = &p.DueID
		}
		// Resolve property_id if omitted
		if p.PropertyID == nil || *p.PropertyID == uuid.Nil {
			var propID uuid.UUID
			if p.DueID != uuid.Nil {
				_ = repo.db.QueryRow(ctx, `SELECT property_id FROM dues WHERE id = $1`, p.DueID).Scan(&propID)
			}
			if propID == uuid.Nil && p.TenantID != uuid.Nil {
				_ = repo.db.QueryRow(ctx, `SELECT property_id FROM tenants WHERE id = $1`, p.TenantID).Scan(&propID)
			}
			if propID != uuid.Nil {
				p.PropertyID = &propID
			} else {
				return fmt.Errorf("cannot create payment: missing or unresolvable property_id")
			}
		}
		// A payment with no due and not marked unapplied has no allocation yet, so its
		// eager ledger mirror would carry a debit with no credit and can never post
		// (outbox "journal lines do not balance"). Such callers must set
		// SkipOutboxEnqueue and enqueue the mirror after allocating.
		if p.DueID == uuid.Nil && !p.IsUnapplied && !p.SkipOutboxEnqueue && p.Amount > 0 {
			return fmt.Errorf("payment %s has no due_id and is not unapplied: set SkipOutboxEnqueue and enqueue the mirror after allocating", p.ID)
		}
		err := repo.db.QueryRow(ctx, `
			INSERT INTO payments (due_id, tenant_id, upi_txn_id, cf_payment_id, provider_payment_id, provider, amount, matched_by, recorded_by, matched_at, raw_note, is_unapplied, created_at, property_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
			RETURNING id`,
			dueIDArg, p.TenantID, p.UPITxnID, p.CFPaymentID, p.ProviderPaymentID, p.Provider, p.Amount, p.MatchedBy, p.RecordedBy, p.MatchedAt, p.RawNote, p.IsUnapplied, p.CreatedAt, p.PropertyID,
		).Scan(&p.ID)
		if err != nil {
			if isUnique(err) {
				return domain.ErrDuplicateTxn
			}
			return err
		}
		if p.DueID != uuid.Nil && p.Amount > 0 && !p.IsUnapplied {
			// Dual-Write Rationale:
			// PaymentRepo.Create is the canonical Go application entrypoint for single-due payments
			// (Cash, Manual, and legacy Single-Due Gateway paths). We explicitly insert into
			// payment_allocations here so that all callers using transactions or mock repos have
			// allocations populated immediately.
			//
			// Concurrently, migration 020 defines PostgreSQL trigger `trg_payments_auto_allocate`
			// which performs the identical ON CONFLICT DO UPDATE upsert on AFTER INSERT ON payments.
			// The trigger exists as an infrastructure-level safety net for direct SQL scripts, manual DB
			// repairs, and raw migration backfills. Because both writes use idempotent
			// ON CONFLICT (payment_id, due_id) DO UPDATE, concurrent or duplicate execution is completely safe.
			_, err = repo.db.Exec(ctx, `
				INSERT INTO payment_allocations (payment_id, due_id, amount_paise, created_at)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (payment_id, due_id) DO UPDATE SET amount_paise = EXCLUDED.amount_paise`,
				p.ID, p.DueID, int64(p.Amount), p.CreatedAt,
			)
			if err != nil {
				return fmt.Errorf("create payment allocation for single due: %w", err)
			}
		}

		// In-transaction universal payment ledger outbox enqueue (C-09)
		if !p.SkipOutboxEnqueue && p.Amount > 0 && p.PropertyID != nil && *p.PropertyID != uuid.Nil {
			var allocs []domain.PaymentAllocationItemPayload
			var unappliedPaise int64
			if p.IsUnapplied {
				unappliedPaise = int64(p.Amount)
			} else if p.DueID != uuid.Nil {
				var dueKind string
				_ = repo.db.QueryRow(ctx, `SELECT kind FROM dues WHERE id = $1`, p.DueID).Scan(&dueKind)
				if dueKind == "" {
					dueKind = string(domain.DueKindRent)
				}
				allocs = append(allocs, domain.PaymentAllocationItemPayload{
					AmountPaise: int64(p.Amount),
					DueKind:     dueKind,
				})
			}
			mirrorPayload := domain.PaymentMirrorPayload{
				PropertyID:     *p.PropertyID,
				PaymentID:      p.ID,
				Allocations:    allocs,
				UnappliedPaise: unappliedPaise,
				IsUnapplied:    p.IsUnapplied,
				MatchedAt:      p.MatchedAt,
				MatchedBy:      p.MatchedBy,
				AmountPaise:    int64(p.Amount),
			}
			rawPayload, err := json.Marshal(mirrorPayload)
			if err != nil {
				return fmt.Errorf("marshal payment mirror outbox payload: %w", err)
			}
			outboxKey := fmt.Sprintf("payment_mirror:%s", p.ID)
			_, err = repo.db.Exec(ctx, `
				INSERT INTO ledger_outbox_events (event_type, property_id, source_id, payload, idempotency_key, created_at)
				VALUES ($1, $2, $3, $4, $5, NOW())
				ON CONFLICT (idempotency_key) DO NOTHING`,
				domain.LedgerOutboxPayment, *p.PropertyID, p.ID, rawPayload, outboxKey,
			)
			if err != nil {
				return fmt.Errorf("enqueue payment ledger outbox: %w", err)
			}
		}
		return nil
	}

	if pool, ok := r.db.(*pgxpool.Pool); ok {
		return WithinTx(ctx, pool, func(tx pgx.Tx) error {
			return insertAll(r.WithTx(tx))
		})
	}
	return insertAll(r)
}

func (r *PaymentRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.Payment, error) {
	return scanPayment(r.db.QueryRow(ctx, `SELECT `+paymentCols+` FROM payments WHERE id=$1`, id))
}

func (r *PaymentRepo) GetByDueID(ctx context.Context, dueID uuid.UUID) (*domain.Payment, error) {
	return scanPayment(r.db.QueryRow(ctx, `
		SELECT `+paymentCols+` 
		FROM payments p
		WHERE p.due_id = $1 
		   OR p.id IN (SELECT pa.payment_id FROM payment_allocations pa WHERE pa.due_id = $1)
		ORDER BY p.created_at DESC
		LIMIT 1`, dueID))
}

func (r *PaymentRepo) GetByUPITxnID(ctx context.Context, txnID string) (*domain.Payment, error) {
	return scanPayment(r.db.QueryRow(ctx, `SELECT `+paymentCols+` FROM payments WHERE upi_txn_id=$1`, txnID))
}

func (r *PaymentRepo) GetByCFPaymentID(ctx context.Context, cfID string) (*domain.Payment, error) {
	return scanPayment(r.db.QueryRow(ctx, `SELECT `+paymentCols+` FROM payments WHERE cf_payment_id=$1`, cfID))
}

func (r *PaymentRepo) GetByProviderPaymentID(ctx context.Context, providerPaymentID string) (*domain.Payment, error) {
	return r.GetByCFPaymentID(ctx, providerPaymentID)
}

func (r *PaymentRepo) ListByProperty(ctx context.Context, propertyID uuid.UUID, matchedBy *domain.MatchedBy) ([]domain.Payment, error) {
	q := `
		SELECT DISTINCT ` + paymentCols + `
		FROM payments p
		WHERE (
			p.property_id = $1
			OR p.due_id IN (SELECT id FROM dues WHERE property_id=$1)
			OR p.id IN (SELECT pa.payment_id FROM payment_allocations pa JOIN dues d ON d.id = pa.due_id WHERE d.property_id=$1)
			OR p.tenant_id IN (SELECT id FROM tenants WHERE property_id=$1)
		)`
	args := []any{propertyID}
	if matchedBy != nil {
		q += ` AND matched_by=$2`
		args = append(args, *matchedBy)
	}
	q += ` ORDER BY matched_at DESC, id DESC LIMIT 50`
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (r *PaymentRepo) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Payment, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+paymentCols+`
		FROM payments WHERE tenant_id=$1 ORDER BY matched_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

// TryAcquirePaymentLock attempts to acquire a short-lived atomic lease for in-flight payment intent creation.
func (r *PaymentRepo) TryAcquirePaymentLock(ctx context.Context, dueID uuid.UUID, ttl time.Duration) (bool, error) {
	now := time.Now().UTC()
	cutoff := now.Add(-ttl)
	tag, err := r.db.Exec(ctx, `
		INSERT INTO payment_in_flight_lock (due_id, locked_at, expires_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (due_id)
		DO UPDATE SET locked_at = EXCLUDED.locked_at, expires_at = EXCLUDED.expires_at
		WHERE payment_in_flight_lock.locked_at <= $4
	`, dueID, now, now.Add(ttl), cutoff)
	if err != nil {
		return false, fmt.Errorf("payment: acquire in-flight lock: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// ReleasePaymentLock removes the in-flight lease for a due.
func (r *PaymentRepo) ReleasePaymentLock(ctx context.Context, dueID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `DELETE FROM payment_in_flight_lock WHERE due_id = $1`, dueID)
	if err != nil {
		return fmt.Errorf("payment: release in-flight lock: %w", err)
	}
	return nil
}

// RecordProcessedEvent inserts into processed_webhook_events inside the calling transaction.
// Returns true if newly inserted, or false if already processed.
func (r *PaymentRepo) RecordProcessedEvent(ctx context.Context, provider, eventType, providerRefID, eventStatus string) (bool, error) {
	if provider == "" {
		provider = "cashfree"
	}
	tag, err := r.db.Exec(ctx, `
		INSERT INTO processed_webhook_events (provider, event_type, provider_reference_id, event_status, created_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (provider, event_type, provider_reference_id, event_status) DO NOTHING
	`, provider, eventType, providerRefID, eventStatus)
	if err != nil {
		return false, fmt.Errorf("payment: record processed event: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// RecordWebhookEvent provides backwards compatibility with older interfaces.
func (r *PaymentRepo) RecordWebhookEvent(ctx context.Context, dedupKey, provider, eventType string) (bool, error) {
	return r.RecordProcessedEvent(ctx, provider, eventType, dedupKey, "")
}

// CreateAllocation links a payment to a due for net paid derived accounting.
func (r *PaymentRepo) CreateAllocation(ctx context.Context, paymentID, dueID uuid.UUID, amountPaise int64) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO payment_allocations (payment_id, due_id, amount_paise, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (payment_id, due_id) DO UPDATE SET amount_paise = EXCLUDED.amount_paise`,
		paymentID, dueID, amountPaise)
	return err
}

func (r *PaymentRepo) ListAllocationsByPayment(ctx context.Context, paymentID uuid.UUID) ([]domain.PaymentAllocation, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, payment_id, due_id, amount_paise, created_at
		FROM payment_allocations WHERE payment_id=$1`, paymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PaymentAllocation
	for rows.Next() {
		var a domain.PaymentAllocation
		if err := rows.Scan(&a.ID, &a.PaymentID, &a.DueID, &a.AmountPaise, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (r *PaymentRepo) CreateWebhookEvent(ctx context.Context, evt *domain.WebhookEvent) error {
	if evt.Provider == "" {
		evt.Provider = "cashfree"
	}
	now := time.Now().UTC()
	evt.ReceivedAt = now
	return r.db.QueryRow(ctx, `
		INSERT INTO webhook_events (provider, event_type, signature, timestamp_header, raw_payload, processing_status, received_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id`,
		evt.Provider, evt.EventType, evt.Signature, evt.TimestampHeader, evt.RawPayload, evt.ProcessingStatus, evt.ReceivedAt,
	).Scan(&evt.ID)
}

func (r *PaymentRepo) UpdateWebhookEventStatus(ctx context.Context, id uuid.UUID, status string, errMsg *string) error {
	_, err := r.db.Exec(ctx, `
		UPDATE webhook_events
		SET processing_status=$2, error_message=$3, processed_at=NOW()
		WHERE id=$1`, id, status, errMsg)
	return err
}

func (r *PaymentRepo) RecordUnmatchedReceipt(ctx context.Context, orderID, paymentID string, intentID *uuid.UUID, amountPaise int64, failureReason string, payload []byte) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO unmatched_gateway_receipts (provider, provider_order_id, provider_payment_id, cf_payment_id, payment_intent_id, amount_paise, failure_reason, raw_payload, created_at)
		VALUES ('cashfree', $1, $2, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (provider, provider_payment_id) DO NOTHING`,
		orderID, paymentID, intentID, amountPaise, failureReason, payload)
	return err
}

const refundCols = `id, payment_id, property_id, provider, provider_refund_id, refund_reference, idempotency_key, amount_paise, status, cf_refund_id, reason, source, initiated_by, raw_response, created_at, updated_at`

func scanRefund(row pgx.Row) (*domain.GatewayRefund, error) {
	var ref domain.GatewayRefund
	err := row.Scan(
		&ref.ID, &ref.PaymentID, &ref.PropertyID, &ref.Provider, &ref.ProviderRefundID,
		&ref.RefundReference, &ref.IdempotencyKey, &ref.AmountPaise, &ref.Status,
		&ref.CFRefundID, &ref.Reason, &ref.Source, &ref.InitiatedBy,
		&ref.RawPayload, &ref.CreatedAt, &ref.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if ref.ProviderRefundID == nil && ref.CFRefundID != nil {
		ref.ProviderRefundID = ref.CFRefundID
	}
	if ref.CFRefundID == nil && ref.ProviderRefundID != nil {
		ref.CFRefundID = ref.ProviderRefundID
	}
	return &ref, nil
}

func (r *PaymentRepo) GetRefundByID(ctx context.Context, id uuid.UUID) (*domain.GatewayRefund, error) {
	return scanRefund(r.db.QueryRow(ctx, `SELECT `+refundCols+` FROM gateway_refunds WHERE id=$1`, id))
}

func (r *PaymentRepo) GetRefundByCFRefundID(ctx context.Context, cfRefundID string) (*domain.GatewayRefund, error) {
	return scanRefund(r.db.QueryRow(ctx, `SELECT `+refundCols+` FROM gateway_refunds WHERE cf_refund_id=$1`, cfRefundID))
}

func (r *PaymentRepo) GetRefundByProviderRefundID(ctx context.Context, providerRefundID string) (*domain.GatewayRefund, error) {
	return r.GetRefundByCFRefundID(ctx, providerRefundID)
}

func (r *PaymentRepo) GetRefundByReference(ctx context.Context, ref string) (*domain.GatewayRefund, error) {
	return scanRefund(r.db.QueryRow(ctx, `SELECT `+refundCols+` FROM gateway_refunds WHERE refund_reference=$1`, ref))
}

func (r *PaymentRepo) GetRefundByPaymentAndIdempotency(ctx context.Context, paymentID uuid.UUID, idempotencyKey string) (*domain.GatewayRefund, error) {
	return scanRefund(r.db.QueryRow(ctx, `SELECT `+refundCols+` FROM gateway_refunds WHERE payment_id=$1 AND idempotency_key=$2`, paymentID, idempotencyKey))
}

func (r *PaymentRepo) GetPaymentRefundedPaise(ctx context.Context, paymentID uuid.UUID) (int64, error) {
	var total int64
	err := r.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(amount_paise), 0)
		FROM gateway_refunds
		WHERE payment_id = $1 AND status NOT IN ('failed', 'cancelled')`, paymentID).Scan(&total)
	return total, err
}

func (r *PaymentRepo) ListRefundsByPayment(ctx context.Context, paymentID uuid.UUID) ([]domain.GatewayRefund, error) {
	rows, err := r.db.Query(ctx, `SELECT `+refundCols+` FROM gateway_refunds WHERE payment_id=$1 ORDER BY created_at ASC LIMIT 100`, paymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.GatewayRefund
	for rows.Next() {
		ref, err := scanRefund(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ref)
	}
	return out, rows.Err()
}

func (r *PaymentRepo) CreateOrUpdateRefund(ctx context.Context, ref *domain.GatewayRefund) error {
	now := time.Now().UTC()
	if ref.CreatedAt.IsZero() {
		ref.CreatedAt = now
	}
	ref.UpdatedAt = now
	if ref.Provider == "" {
		ref.Provider = "cashfree"
	}
	if ref.Provider == "cashfree" {
		if ref.CFRefundID == nil && ref.ProviderRefundID != nil {
			ref.CFRefundID = ref.ProviderRefundID
		}
		if ref.ProviderRefundID == nil && ref.CFRefundID != nil {
			ref.ProviderRefundID = ref.CFRefundID
		}
	}
	if ref.ID == uuid.Nil {
		ref.ID = uuid.New()
	}
	return r.db.QueryRow(ctx, `
		INSERT INTO gateway_refunds (id, payment_id, property_id, provider, provider_refund_id, cf_refund_id, refund_reference, idempotency_key, amount_paise, status, source, initiated_by, reason, raw_response, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
		ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status,
			cf_refund_id = COALESCE(EXCLUDED.cf_refund_id, gateway_refunds.cf_refund_id),
			provider_refund_id = COALESCE(EXCLUDED.provider_refund_id, gateway_refunds.provider_refund_id),
			amount_paise = EXCLUDED.amount_paise,
			raw_response = COALESCE(EXCLUDED.raw_response, gateway_refunds.raw_response),
			updated_at = NOW()
		RETURNING id`,
		ref.ID, ref.PaymentID, ref.PropertyID, ref.Provider, ref.ProviderRefundID, ref.CFRefundID,
		ref.RefundReference, ref.IdempotencyKey, ref.AmountPaise, ref.Status, ref.Source,
		ref.InitiatedBy, ref.Reason, ref.RawPayload, ref.CreatedAt, ref.UpdatedAt,
	).Scan(&ref.ID)
}

func (r *PaymentRepo) CreateRefundAllocation(ctx context.Context, alloc *domain.RefundAllocation) error {
	dueID := uuid.Nil
	if alloc.DueID != nil {
		dueID = *alloc.DueID
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO refund_allocations (refund_id, due_id, amount_paise, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (refund_id, due_id) DO UPDATE SET amount_paise = EXCLUDED.amount_paise`,
		alloc.RefundID, dueID, alloc.AmountPaise)
	return err
}

func (r *PaymentRepo) GetDueNetPaidPaise(ctx context.Context, dueID uuid.UUID) (int64, error) {
	var netPaid int64
	err := r.db.QueryRow(ctx, `
		SELECT 
			COALESCE((SELECT SUM(amount_paise) FROM payment_allocations WHERE due_id = $1), 0) -
			COALESCE((SELECT SUM(ra.amount_paise) FROM refund_allocations ra JOIN gateway_refunds gr ON gr.id = ra.refund_id WHERE ra.due_id = $1 AND gr.status = 'succeeded'), 0) -
			COALESCE((SELECT SUM(amount_paise) FROM departure_due_adjustments WHERE due_id = $1), 0)`, dueID).Scan(&netPaid)
	return netPaid, err
}

func (r *PaymentRepo) GetPaymentDueRefundedPaise(ctx context.Context, paymentID, dueID, excludeRefundID uuid.UUID) (int64, error) {
	var refunded int64
	err := r.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(ra.amount_paise), 0)
		FROM refund_allocations ra
		JOIN gateway_refunds gr ON gr.id = ra.refund_id
		WHERE ra.due_id = $1 AND gr.payment_id = $2 AND gr.id != $3 AND gr.status = 'succeeded'`,
		dueID, paymentID, excludeRefundID).Scan(&refunded)
	return refunded, err
}

func (r *PaymentRepo) GetRefundDueIDs(ctx context.Context, refundID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := r.db.Query(ctx, `
		SELECT DISTINCT due_id 
		FROM refund_allocations 
		WHERE refund_id = $1 AND due_id IS NOT NULL`, refundID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err == nil && id != uuid.Nil {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

func (r *PaymentRepo) ListStaleNonTerminalRefunds(ctx context.Context, olderThan time.Time) ([]domain.GatewayRefund, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+refundCols+`
		FROM gateway_refunds
		WHERE status IN ('initiated', 'pending', 'on_hold') AND created_at < $1
		ORDER BY created_at ASC`, olderThan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.GatewayRefund
	for rows.Next() {
		ref, err := scanRefund(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ref)
	}
	return out, rows.Err()
}

func (r *PaymentRepo) ReconcileRefundTransition(ctx context.Context, ref *domain.GatewayRefund, newStatus string) error {
	reconcile := func(txRepo *PaymentRepo, tx pgx.Tx) error {
		p, err := txRepo.GetByID(ctx, ref.PaymentID)
		if err != nil || p == nil {
			return fmt.Errorf("reconcile refund payment lookup: %w", err)
		}

		var dummy uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM tenants WHERE id = $1 FOR UPDATE`, p.TenantID).Scan(&dummy); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("lock tenant for refund reconcile: %w", err)
		}

		txDueRepo := NewDueRepo(tx)
		var primaryDue *domain.Due
		if p.DueID != uuid.Nil {
			primaryDue, _ = txDueRepo.GetByIDForUpdate(ctx, p.DueID)
		}

		if err := tx.QueryRow(ctx, `SELECT id FROM payments WHERE id = $1 FOR UPDATE`, p.ID).Scan(&dummy); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("lock payment for refund reconcile: %w", err)
		}

		if err := tx.QueryRow(ctx, `SELECT id FROM gateway_refunds WHERE id = $1 FOR UPDATE`, ref.ID).Scan(&dummy); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("lock gateway refund for reconcile: %w", err)
		}

		ref.Status = newStatus
		ref.UpdatedAt = time.Now().UTC()
		if err := txRepo.CreateOrUpdateRefund(ctx, ref); err != nil {
			return fmt.Errorf("update refund status in reconcile: %w", err)
		}

		switch newStatus {
		case "succeeded":
			type allocItem struct {
				dueID       uuid.UUID
				amountPaise int64
			}
			var existingAllocs []allocItem
			allocRows, qErr := tx.Query(ctx, `SELECT due_id, amount_paise FROM refund_allocations WHERE refund_id = $1`, ref.ID)
			if qErr == nil {
				for allocRows.Next() {
					var item allocItem
					if err := allocRows.Scan(&item.dueID, &item.amountPaise); err == nil && item.dueID != uuid.Nil {
						existingAllocs = append(existingAllocs, item)
					}
				}
				allocRows.Close()
			}

			var mirrorAllocs []domain.RefundAllocationItemPayload
			if len(existingAllocs) > 0 {
				for _, item := range existingAllocs {
					targetDue, err := txDueRepo.GetByIDForUpdate(ctx, item.dueID)
					if err == nil && targetDue != nil {
						if err := txRepo.recomputeDueStatusUnderLock(ctx, txDueRepo, targetDue); err != nil {
							return err
						}
						mirrorAllocs = append(mirrorAllocs, domain.RefundAllocationItemPayload{
							AmountPaise: item.amountPaise,
							DueKind:     string(targetDue.Kind),
						})
					}
				}
			} else if !p.IsUnapplied {
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

				remRefund := ref.AmountPaise
				for _, pa := range pAllocs {
					if remRefund <= 0 {
						break
					}
					prevRefunded, err := txRepo.GetPaymentDueRefundedPaise(ctx, p.ID, pa.dueID, ref.ID)
					if err != nil {
						return err
					}
					remAlloc := pa.amountPaise - prevRefunded
					if remAlloc <= 0 {
						continue
					}
					netPaid, err := txRepo.GetDueNetPaidPaise(ctx, pa.dueID)
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
					if err := txRepo.CreateRefundAllocation(ctx, &domain.RefundAllocation{
						RefundID:    ref.ID,
						DueID:       &dueIDCopy,
						AmountPaise: allocAmt,
					}); err != nil {
						return err
					}
					targetDue, err := txDueRepo.GetByIDForUpdate(ctx, pa.dueID)
					if err == nil && targetDue != nil {
						if err := txRepo.recomputeDueStatusUnderLock(ctx, txDueRepo, targetDue); err != nil {
							return err
						}
						mirrorAllocs = append(mirrorAllocs, domain.RefundAllocationItemPayload{
							AmountPaise: allocAmt,
							DueKind:     string(pa.kind),
						})
					}
					remRefund -= allocAmt
				}
			}

			if len(mirrorAllocs) == 0 && primaryDue != nil && !p.IsUnapplied {
				prevRefunded, err := txRepo.GetPaymentDueRefundedPaise(ctx, p.ID, primaryDue.ID, ref.ID)
				if err != nil {
					return err
				}
				netPaid, err := txRepo.GetDueNetPaidPaise(ctx, primaryDue.ID)
				if err != nil {
					return err
				}
				maxAlloc := p.Amount - prevRefunded
				if netPaid < maxAlloc {
					maxAlloc = netPaid
				}
				allocAmt := ref.AmountPaise
				if allocAmt > maxAlloc {
					allocAmt = maxAlloc
				}
				if allocAmt > 0 {
					if err := txRepo.CreateRefundAllocation(ctx, &domain.RefundAllocation{
						RefundID:    ref.ID,
						DueID:       &primaryDue.ID,
						AmountPaise: allocAmt,
					}); err != nil {
						return err
					}
					if err := txRepo.recomputeDueStatusUnderLock(ctx, txDueRepo, primaryDue); err != nil {
						return err
					}
					mirrorAllocs = append(mirrorAllocs, domain.RefundAllocationItemPayload{
						AmountPaise: allocAmt,
						DueKind:     string(primaryDue.Kind),
					})
				}
			}

			propID := ref.PropertyID
			if propID == uuid.Nil && primaryDue != nil {
				propID = primaryDue.PropertyID
			}
			if propID == uuid.Nil && p.PropertyID != nil {
				propID = *p.PropertyID
			}
			dueKind := string(domain.DueKindRent)
			if len(mirrorAllocs) > 0 {
				dueKind = mirrorAllocs[0].DueKind
			} else if primaryDue != nil {
				dueKind = string(primaryDue.Kind)
			}
			refundPayload := domain.RefundMirrorPayload{
				PropertyID:  propID,
				RefundID:    ref.ID,
				PaymentID:   p.ID,
				AmountPaise: ref.AmountPaise,
				IsUnapplied: p.IsUnapplied,
				Allocations: mirrorAllocs,
				DueKind:     dueKind,
				OccurredAt:  time.Now().UTC(),
			}
			rawPayload, err := json.Marshal(refundPayload)
			if err != nil {
				return fmt.Errorf("marshal refund mirror outbox: %w", err)
			}
			outboxKey := fmt.Sprintf("refund_mirror:%s", ref.ID)
			_, err = tx.Exec(ctx, `
				INSERT INTO ledger_outbox_events (event_type, property_id, source_id, payload, idempotency_key, created_at)
				VALUES ($1, $2, $3, $4, $5, NOW())
				ON CONFLICT (idempotency_key) DO NOTHING`,
				domain.LedgerOutboxRefund, propID, ref.ID, rawPayload, outboxKey,
			)
			if err != nil {
				return fmt.Errorf("enqueue refund ledger outbox in reconcile: %w", err)
			}

		case "failed", "cancelled":
			if _, err := tx.Exec(ctx, `DELETE FROM refund_allocations WHERE refund_id = $1`, ref.ID); err != nil {
				return fmt.Errorf("delete refund allocations in reconcile: %w", err)
			}
		}
		return nil
	}

	if pool, ok := r.db.(*pgxpool.Pool); ok {
		return WithinTx(ctx, pool, func(tx pgx.Tx) error {
			return reconcile(r.WithTx(tx), tx)
		})
	}
	if tx, ok := r.db.(pgx.Tx); ok {
		return reconcile(r, tx)
	}
	return errors.New("database connection does not support transactions")
}

func (r *PaymentRepo) recomputeDueStatusUnderLock(ctx context.Context, txDueRepo *DueRepo, d *domain.Due) error {
	netPaid, err := r.GetDueNetPaidPaise(ctx, d.ID)
	if err != nil {
		return err
	}
	ceiling := d.OriginalAmount
	if d.ContractualCeilingPaise != nil && *d.ContractualCeilingPaise > 0 {
		ceiling = *d.ContractualCeilingPaise
	}
	newStatus, newAmount := domain.RecomputeDueStatusMath(netPaid, ceiling)
	d.Status = newStatus
	d.Amount = newAmount
	if d.Status != domain.DueStatusPaid {
		d.PaidAt = nil
	}
	return txDueRepo.Update(ctx, d)
}

func (r *PaymentRepo) GetPaymentOrderID(ctx context.Context, paymentID uuid.UUID) (string, error) {
	var orderID string
	err := r.db.QueryRow(ctx, `
		SELECT COALESCE(
			(SELECT pi.provider_order_id FROM payments p JOIN payment_intents pi ON pi.due_id = p.due_id WHERE p.id = $1 LIMIT 1),
			''
		)`, paymentID).Scan(&orderID)
	return orderID, err
}

func (r *PaymentRepo) HasCorrection(ctx context.Context, paymentID uuid.UUID) (bool, error) {
	var exists bool
	err := r.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM financial_corrections WHERE original_payment_id = $1)`, paymentID).Scan(&exists)
	return exists, err
}

func (r *PaymentRepo) RecordCorrection(ctx context.Context, c *domain.FinancialCorrection) error {
	now := time.Now().UTC()
	if c.OccurredAt.IsZero() {
		c.OccurredAt = now
	}
	c.CreatedAt = now
	err := r.db.QueryRow(ctx, `
		INSERT INTO financial_corrections (property_id, original_payment_id, reversal_payment_id, corrected_payment_id, reason, corrected_by, occurred_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id`,
		c.PropertyID, c.OriginalPaymentID, c.ReversalPaymentID, c.CorrectedPaymentID, c.Reason, c.CorrectedBy, c.OccurredAt, c.CreatedAt,
	).Scan(&c.ID)
	if err != nil {
		return err
	}

	// In-transaction universal correction outbox enqueue (C-10)
	var origAmt int64
	_ = r.db.QueryRow(ctx, `SELECT amount FROM payments WHERE id = $1`, c.OriginalPaymentID).Scan(&origAmt)
	if origAmt > 0 {
		payload, err := json.Marshal(domain.CorrectionMirrorPayload{
			PropertyID:         c.PropertyID,
			CorrectionID:       c.ID,
			OriginalPaymentID:  c.OriginalPaymentID,
			ReversalPaymentID:  c.ReversalPaymentID,
			CorrectedPaymentID: c.CorrectedPaymentID,
			AmountPaise:        origAmt,
			OccurredAt:         c.OccurredAt,
		})
		if err != nil {
			return fmt.Errorf("marshal correction mirror outbox payload: %w", err)
		}
		outboxKey := fmt.Sprintf("correction_mirror:%s", c.ID)
		_, err = r.db.Exec(ctx, `
			INSERT INTO ledger_outbox_events (event_type, property_id, source_id, payload, idempotency_key, created_at)
			VALUES ($1, $2, $3, $4, $5, NOW())
			ON CONFLICT (idempotency_key) DO NOTHING`,
			domain.LedgerOutboxCorrection, c.PropertyID, c.ID, payload, outboxKey,
		)
		if err != nil {
			return fmt.Errorf("enqueue correction ledger outbox: %w", err)
		}
	}
	return nil
}

func (r *PaymentRepo) ListCorrections(ctx context.Context, propertyID uuid.UUID) ([]domain.FinancialCorrection, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, property_id, original_payment_id, reversal_payment_id, corrected_payment_id, reason, corrected_by, occurred_at, created_at
		FROM financial_corrections
		WHERE property_id = $1
		ORDER BY occurred_at DESC`, propertyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.FinancialCorrection
	for rows.Next() {
		var c domain.FinancialCorrection
		if err := rows.Scan(&c.ID, &c.PropertyID, &c.OriginalPaymentID, &c.ReversalPaymentID, &c.CorrectedPaymentID, &c.Reason, &c.CorrectedBy, &c.OccurredAt, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *PaymentRepo) ListByPropertyPaginated(ctx context.Context, propertyID uuid.UUID, matchedBy *domain.MatchedBy, limit int, before *time.Time, beforeID *uuid.UUID) ([]domain.Payment, error) {
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	q := `
		SELECT DISTINCT ` + paymentCols + `
		FROM payments p
		WHERE (
			p.due_id IN (SELECT id FROM dues WHERE property_id=$1)
			OR p.id IN (SELECT pa.payment_id FROM payment_allocations pa JOIN dues d ON d.id = pa.due_id WHERE d.property_id=$1)
			OR p.tenant_id IN (SELECT id FROM tenants WHERE property_id=$1)
		)`
	args := []any{propertyID}
	argIdx := 2
	if matchedBy != nil {
		q += fmt.Sprintf(" AND matched_by=$%d", argIdx)
		args = append(args, *matchedBy)
		argIdx++
	}
	// REQ-PERF-005: compound keyset cursor prevents duplicates/skips at equal timestamps.
	if before != nil && !before.IsZero() && beforeID != nil {
		q += fmt.Sprintf(" AND (matched_at, id) < ($%d, $%d)", argIdx, argIdx+1)
		args = append(args, *before, *beforeID)
		argIdx += 2
	} else if before != nil && !before.IsZero() {
		// Backward-compat: timestamp-only when no beforeID provided.
		q += fmt.Sprintf(" AND matched_at < $%d", argIdx)
		args = append(args, *before)
		argIdx++
	}
	q += fmt.Sprintf(" ORDER BY matched_at DESC, id DESC LIMIT $%d", argIdx)
	args = append(args, limit)

	rows, err := r.db.Query(ctx, q, args...)

	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Payment
	for rows.Next() {
		p, err := scanPayment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}
