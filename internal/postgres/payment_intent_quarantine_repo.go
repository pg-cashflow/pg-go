package postgres

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// PaymentIntentQuarantineRepo stores durable recovery state for superseded
// Cashfree orders. It must use the maintenance connection pool.
type PaymentIntentQuarantineRepo struct {
	pool *pgxpool.Pool
}

func NewPaymentIntentQuarantineRepo(pool *pgxpool.Pool) *PaymentIntentQuarantineRepo {
	return &PaymentIntentQuarantineRepo{pool: pool}
}

func (r *PaymentIntentQuarantineRepo) ListReady(ctx context.Context, limit int) ([]domain.PaymentIntentQuarantine, error) {
	if r == nil || r.pool == nil {
		return nil, errors.New("payment intent quarantine: database pool unavailable")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
		SELECT id, payment_intent_id, COALESCE(due_id, '00000000-0000-0000-0000-000000000000'::uuid),
		       COALESCE(property_id, '00000000-0000-0000-0000-000000000000'::uuid), due_ids,
		       provider_order_id, amount_paise, original_status, quarantine_reason,
		       status, next_check_at, attempt_count, last_provider_status, last_error,
		       created_at, updated_at
		FROM payment_intent_quarantine
		WHERE status <> 'resolved' AND next_check_at <= NOW()
		ORDER BY next_check_at ASC, created_at ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("list payment intent quarantine: %w", err)
	}
	defer rows.Close()

	items := make([]domain.PaymentIntentQuarantine, 0)
	for rows.Next() {
		var item domain.PaymentIntentQuarantine
		if err := rows.Scan(
			&item.ID, &item.PaymentIntentID, &item.DueID, &item.PropertyID, &item.DueIDs,
			&item.ProviderOrderID, &item.AmountPaise, &item.OriginalStatus, &item.QuarantineReason,
			&item.Status, &item.NextCheckAt, &item.AttemptCount, &item.LastProviderStatus,
			&item.LastError, &item.CreatedAt, &item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan payment intent quarantine: %w", err)
		}
		item.Intent = domain.PaymentIntent{
			ID:              item.PaymentIntentID,
			DueID:           item.DueID,
			Provider:        "cashfree",
			ProviderOrderID: item.ProviderOrderID,
			AmountPaise:     item.AmountPaise,
			Status:          domain.IntentSuperseded,
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate payment intent quarantine: %w", err)
	}
	return items, nil
}

// CountNeedsReviewByProperty returns the number of quarantined orders in 'needs_review' status
// for an individual property.
func (r *PaymentIntentQuarantineRepo) CountNeedsReviewByProperty(ctx context.Context, propertyID uuid.UUID) (int, error) {
	if r == nil || r.pool == nil {
		return 0, errors.New("payment intent quarantine: database pool unavailable")
	}
	var count int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM payment_intent_quarantine
		WHERE property_id = $1 AND status = 'needs_review'`,
		propertyID,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count needs_review payment intent quarantine: %w", err)
	}
	return count, nil
}

// ListByProperty returns quarantined orders for a property, optionally filtered by status.
func (r *PaymentIntentQuarantineRepo) ListByProperty(ctx context.Context, propertyID uuid.UUID, status string, limit int) ([]domain.PaymentIntentQuarantine, error) {
	if r == nil || r.pool == nil {
		return nil, errors.New("payment intent quarantine: database pool unavailable")
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var rows pgx.Rows
	var err error
	status = strings.TrimSpace(status)
	if status != "" {
		rows, err = r.pool.Query(ctx, `
			SELECT id, payment_intent_id, COALESCE(due_id, '00000000-0000-0000-0000-000000000000'::uuid),
			       COALESCE(property_id, '00000000-0000-0000-0000-000000000000'::uuid), due_ids,
			       provider_order_id, amount_paise, original_status, quarantine_reason,
			       status, next_check_at, attempt_count, last_provider_status, last_error,
			       created_at, updated_at
			FROM payment_intent_quarantine
			WHERE property_id = $1 AND status = $2
			ORDER BY created_at DESC
			LIMIT $3`, propertyID, status, limit)
	} else {
		rows, err = r.pool.Query(ctx, `
			SELECT id, payment_intent_id, COALESCE(due_id, '00000000-0000-0000-0000-000000000000'::uuid),
			       COALESCE(property_id, '00000000-0000-0000-0000-000000000000'::uuid), due_ids,
			       provider_order_id, amount_paise, original_status, quarantine_reason,
			       status, next_check_at, attempt_count, last_provider_status, last_error,
			       created_at, updated_at
			FROM payment_intent_quarantine
			WHERE property_id = $1
			ORDER BY created_at DESC
			LIMIT $2`, propertyID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("list payment intent quarantine by property: %w", err)
	}
	defer rows.Close()

	items := make([]domain.PaymentIntentQuarantine, 0)
	for rows.Next() {
		var item domain.PaymentIntentQuarantine
		if err := rows.Scan(
			&item.ID, &item.PaymentIntentID, &item.DueID, &item.PropertyID, &item.DueIDs,
			&item.ProviderOrderID, &item.AmountPaise, &item.OriginalStatus, &item.QuarantineReason,
			&item.Status, &item.NextCheckAt, &item.AttemptCount, &item.LastProviderStatus,
			&item.LastError, &item.CreatedAt, &item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan payment intent quarantine: %w", err)
		}
		item.Intent = domain.PaymentIntent{
			ID:              item.PaymentIntentID,
			DueID:           item.DueID,
			Provider:        "cashfree",
			ProviderOrderID: item.ProviderOrderID,
			AmountPaise:     item.AmountPaise,
			Status:          domain.IntentSuperseded,
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate payment intent quarantine: %w", err)
	}
	return items, nil
}

func (r *PaymentIntentQuarantineRepo) RecordAttempt(
	ctx context.Context,
	id int64,
	status string,
	providerStatus string,
	cause error,
	nextCheck time.Time,
) error {
	if r == nil || r.pool == nil {
		return errors.New("payment intent quarantine: database pool unavailable")
	}
	switch status {
	case "pending", "termination_requested", "needs_review":
	default:
		return fmt.Errorf("payment intent quarantine: invalid retry status %q", status)
	}
	var provider *string
	if s := strings.TrimSpace(providerStatus); s != "" {
		provider = &s
	}
	var lastErr *string
	if cause != nil {
		// Keep provider error text bounded. It must not contain request headers or secrets.
		message := cause.Error()
		if len(message) > 1000 {
			message = message[:1000]
		}
		lastErr = &message
	}
	_, err := r.pool.Exec(ctx, `
		UPDATE payment_intent_quarantine
		SET status = $2,
		    terminal_confirmation_count = CASE
		        WHEN ($3::TEXT) IS NULL THEN terminal_confirmation_count
		        WHEN UPPER(BTRIM($3::TEXT)) IN ('PAID', 'EXPIRED', 'TERMINATED') AND UPPER(BTRIM($3::TEXT)) = last_provider_status
		            THEN LEAST(terminal_confirmation_count + 1, 2)
		        WHEN UPPER(BTRIM($3::TEXT)) IN ('PAID', 'EXPIRED', 'TERMINATED') THEN 1
		        ELSE 0
		    END,
		    last_provider_status = COALESCE(UPPER(BTRIM($3::TEXT)), last_provider_status),
		    last_error = $4::TEXT,
		    attempt_count = attempt_count + 1,
		    last_attempt_at = NOW(),
		    next_check_at = $5,
		    updated_at = NOW()
		WHERE id = $1 AND status <> 'resolved'`,
		id, status, provider, lastErr, nextCheck.UTC(),
	)
	if err != nil {
		return fmt.Errorf("record payment intent quarantine attempt: %w", err)
	}
	return nil
}

func (r *PaymentIntentQuarantineRepo) MarkResolved(ctx context.Context, id int64, providerStatus string) error {
	if r == nil || r.pool == nil {
		return errors.New("payment intent quarantine: database pool unavailable")
	}
	result, err := r.pool.Exec(ctx, `
		UPDATE payment_intent_quarantine q
		SET status = 'resolved',
		    terminal_confirmation_count = LEAST(q.terminal_confirmation_count + 1, 2),
		    last_provider_status = COALESCE(NULLIF(UPPER(BTRIM($2)), ''), q.last_provider_status),
		    last_error = NULL,
		    resolved_at = NOW(),
		    updated_at = NOW()
		WHERE q.id = $1
		  AND q.status <> 'resolved'
		  AND COALESCE(NULLIF(UPPER(BTRIM($2)), ''), q.last_provider_status) IN ('PAID', 'EXPIRED', 'TERMINATED')
		  AND q.terminal_confirmation_count >= 1
		  AND COALESCE(NULLIF(UPPER(BTRIM($2)), ''), q.last_provider_status) = q.last_provider_status
		  AND NOT EXISTS (
		      SELECT 1
		      FROM unmatched_gateway_receipts receipt
		      WHERE (receipt.payment_intent_id = q.payment_intent_id OR receipt.provider_order_id = q.provider_order_id)
		        AND receipt.status <> 'resolved'
		  )`, id, strings.TrimSpace(providerStatus))
	if err != nil {
		return fmt.Errorf("resolve payment intent quarantine: %w", err)
	}
	if result.RowsAffected() == 0 {
		var status string
		var lastProviderStatus *string
		var openReceipts bool
		var terminalConfirmations int16
		lookupErr := r.pool.QueryRow(ctx, `
			SELECT q.status, q.last_provider_status, q.terminal_confirmation_count, EXISTS (
			    SELECT 1 FROM unmatched_gateway_receipts receipt
			    WHERE (receipt.payment_intent_id = q.payment_intent_id OR receipt.provider_order_id = q.provider_order_id)
			      AND receipt.status <> 'resolved'
			)
			FROM payment_intent_quarantine q WHERE q.id = $1`, id).Scan(&status, &lastProviderStatus, &terminalConfirmations, &openReceipts)
		if lookupErr != nil {
			return fmt.Errorf("resolve payment intent quarantine affected no row: %w", lookupErr)
		}
		if status == "resolved" {
			return nil
		}
		if openReceipts {
			return errors.New("payment intent quarantine has an unresolved unmatched receipt; owner resolution is required")
		}
		if lastProviderStatus == nil || !isTerminalCashfreeOrderStatus(*lastProviderStatus) || terminalConfirmations < 1 {
			return errors.New("payment intent quarantine requires a second matching provider-confirmed terminal order state")
		}
		return errors.New("payment intent quarantine was not resolved")
	}
	return nil
}

func isTerminalCashfreeOrderStatus(status string) bool {
	switch strings.ToUpper(strings.TrimSpace(status)) {
	case "PAID", "EXPIRED", "TERMINATED":
		return true
	default:
		return false
	}
}
