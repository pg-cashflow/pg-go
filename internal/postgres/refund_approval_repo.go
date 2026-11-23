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
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

var (
	ErrRefundApprovalScopeRequired = errors.New("postgres: refund approval property scope is required")
	ErrRefundApprovalScopeMismatch = errors.New("postgres: refund approval property scope mismatch")
	ErrManagerRefundsDisabled      = errors.New("postgres: manager refunds disabled by property owner")
	ErrManagerRefundLimitExceeded  = errors.New("postgres: manager refund amount exceeds property limit")
)

// validateManagerRefundPolicy checks the owner-controlled manager delegation policy.
func validateManagerRefundPolicy(enabled bool, limit *int64, amountPaise int64) error {
	if !enabled {
		return ErrManagerRefundsDisabled
	}
	if limit != nil && amountPaise > *limit {
		return ErrManagerRefundLimitExceeded
	}
	return nil
}

type RefundApprovalRepo struct {
	pool *pgxpool.Pool
}

func NewRefundApprovalRepo(pool *pgxpool.Pool) *RefundApprovalRepo {
	return &RefundApprovalRepo{pool: pool}
}

// refundApprovalScope requires a verified property scope from the request context.
// The explicit property ID, when supplied, must match that scope. This keeps the
// repository fail-closed even when a caller forgets an SQL property predicate.
func refundApprovalScope(ctx context.Context, explicitPropertyID uuid.UUID) (uuid.UUID, error) {
	scopedPropertyID, ok := requestscope.PropertyIDFromContext(ctx)
	if !ok || scopedPropertyID == uuid.Nil {
		return uuid.Nil, ErrRefundApprovalScopeRequired
	}
	if explicitPropertyID != uuid.Nil && explicitPropertyID != scopedPropertyID {
		return uuid.Nil, ErrRefundApprovalScopeMismatch
	}
	return scopedPropertyID, nil
}

func (r *RefundApprovalRepo) withScopedTx(ctx context.Context, propertyID uuid.UUID, fn func(pgx.Tx) error) error {
	if r.pool == nil {
		return errors.New("database pool unavailable")
	}
	return WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
		// Set role and scope inside this transaction. SET LOCAL semantics are safe
		// with transaction pooling and cannot leak to the next pool borrower.
		if _, err := tx.Exec(ctx,
			"SELECT set_config('role', 'pgapp_app', true), set_config('app.current_property_id', $1, true)",
			propertyID.String(),
		); err != nil {
			return fmt.Errorf("set refund approval property scope: %w", err)
		}
		return fn(tx)
	})
}

func (r *RefundApprovalRepo) CreateRequest(ctx context.Context, req *domain.RefundApprovalRequest) error {
	if req == nil {
		return errors.New("refund approval request is required")
	}
	propertyID, scopeErr := refundApprovalScope(ctx, req.PropertyID)
	if scopeErr != nil {
		return scopeErr
	}
	// Normalize omitted IDs to the verified scope. Never insert a nil or caller-
	// supplied foreign property ID after the scope check.
	req.PropertyID = propertyID
	if req.ID == uuid.Nil {
		req.ID = uuid.New()
	}
	now := time.Now().UTC()
	if req.CreatedAt.IsZero() {
		req.CreatedAt = now
	}
	req.UpdatedAt = now

	approvalsJSON, err := json.Marshal(req.Approvals)
	if err != nil {
		approvalsJSON = []byte("[]")
	}

	return r.withScopedTx(ctx, propertyID, func(tx pgx.Tx) error {
		if req.RequestedByRole == string(domain.RoleManager) || req.RequestedByRole == "manager" {
			// Preserve the auth lock order: user -> capability -> property policy.
			if err := AssertCapabilityTx(ctx, tx, req.PropertyID, req.RequestedBy, req.RequestedByRole, "refunds.request"); err != nil {
				return err
			}

			// Lock the policy row until the approval request is inserted. This makes a concurrent
			// owner change wait, so the request is evaluated against one effective policy.
			var enabled bool
			var limit *int64
			err := tx.QueryRow(ctx, `
				SELECT COALESCE(allow_manager_refunds, false), manager_refund_limit_paise
				FROM property_onboarding_settings
				WHERE property_id = $1
				FOR SHARE`, req.PropertyID,
			).Scan(&enabled, &limit)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrManagerRefundsDisabled
			}
			if err != nil {
				return fmt.Errorf("load manager refund policy: %w", err)
			}
			if err := validateManagerRefundPolicy(enabled, limit, req.AmountPaise); err != nil {
				return err
			}
		}

		_, err := tx.Exec(ctx, `
			INSERT INTO refund_approval_requests (
				id, property_id, payment_id, tenant_id, amount_paise, reason, idempotency_key,
				requested_by, requested_by_role, status, required_approvals, material_hash,
				approvals, rejected_by, rejected_at, rejection_reason, refund_id, created_at, updated_at
			) VALUES (
				$1, $2, $3, $4, $5, $6, $7,
				$8, $9, $10, $11, $12,
				$13, $14, $15, $16, $17, $18, $19
			)`,
			req.ID, req.PropertyID, req.PaymentID, req.TenantID, req.AmountPaise, req.Reason, req.IdempotencyKey,
			req.RequestedBy, req.RequestedByRole, req.Status, req.RequiredApprovals, req.MaterialHash,
			approvalsJSON, req.RejectedBy, req.RejectedAt, req.RejectionReason, req.RefundID, req.CreatedAt, req.UpdatedAt,
		)
		return err
	})
}

func (r *RefundApprovalRepo) scanRow(row pgx.Row) (*domain.RefundApprovalRequest, error) {
	var req domain.RefundApprovalRequest
	var approvalsJSON []byte
	err := row.Scan(
		&req.ID, &req.PropertyID, &req.PaymentID, &req.TenantID, &req.AmountPaise, &req.Reason, &req.IdempotencyKey,
		&req.RequestedBy, &req.RequestedByRole, &req.Status, &req.RequiredApprovals, &req.MaterialHash,
		&approvalsJSON, &req.RejectedBy, &req.RejectedAt, &req.RejectionReason, &req.RefundID, &req.CreatedAt, &req.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, err
	}
	if len(approvalsJSON) > 0 {
		_ = json.Unmarshal(approvalsJSON, &req.Approvals)
	}
	if req.Approvals == nil {
		req.Approvals = []domain.RefundApprovalDecision{}
	}
	return &req, nil
}

const refundApprovalCols = `
	id, property_id, payment_id, tenant_id, amount_paise, reason, idempotency_key,
	requested_by, requested_by_role, status, required_approvals, material_hash,
	approvals, rejected_by, rejected_at, rejection_reason, refund_id, created_at, updated_at
`

func (r *RefundApprovalRepo) GetRequestByID(ctx context.Context, id uuid.UUID) (*domain.RefundApprovalRequest, error) {
	propertyID, err := refundApprovalScope(ctx, uuid.Nil)
	if err != nil {
		return nil, err
	}
	var out *domain.RefundApprovalRequest
	err = r.withScopedTx(ctx, propertyID, func(tx pgx.Tx) error {
		var scanErr error
		out, scanErr = r.scanRow(tx.QueryRow(ctx,
			`SELECT `+refundApprovalCols+` FROM refund_approval_requests WHERE id = $1 AND property_id = $2`,
			id, propertyID,
		))
		return scanErr
	})
	return out, err
}

func (r *RefundApprovalRepo) GetRequestByPaymentAndIdempotency(ctx context.Context, paymentID uuid.UUID, idempotencyKey string) (*domain.RefundApprovalRequest, error) {
	propertyID, err := refundApprovalScope(ctx, uuid.Nil)
	if err != nil {
		return nil, err
	}
	var out *domain.RefundApprovalRequest
	err = r.withScopedTx(ctx, propertyID, func(tx pgx.Tx) error {
		var scanErr error
		out, scanErr = r.scanRow(tx.QueryRow(ctx,
			`SELECT `+refundApprovalCols+` FROM refund_approval_requests WHERE payment_id = $1 AND idempotency_key = $2 AND property_id = $3`,
			paymentID, idempotencyKey, propertyID,
		))
		return scanErr
	})
	return out, err
}

func (r *RefundApprovalRepo) ListRequestsByPayment(ctx context.Context, paymentID uuid.UUID) ([]domain.RefundApprovalRequest, error) {
	propertyID, err := refundApprovalScope(ctx, uuid.Nil)
	if err != nil {
		return nil, err
	}
	var list []domain.RefundApprovalRequest
	err = r.withScopedTx(ctx, propertyID, func(tx pgx.Tx) error {
		rows, queryErr := tx.Query(ctx,
			`SELECT `+refundApprovalCols+` FROM refund_approval_requests WHERE payment_id = $1 AND property_id = $2 ORDER BY created_at DESC`,
			paymentID, propertyID,
		)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		for rows.Next() {
			req, scanErr := r.scanRow(rows)
			if scanErr != nil {
				return scanErr
			}
			list = append(list, *req)
		}
		return rows.Err()
	})
	return list, err
}

func (r *RefundApprovalRepo) ListRequestsByProperty(ctx context.Context, propertyID uuid.UUID, status string) ([]domain.RefundApprovalRequest, error) {
	scopedPropertyID, err := refundApprovalScope(ctx, propertyID)
	if err != nil {
		return nil, err
	}
	var list []domain.RefundApprovalRequest
	err = r.withScopedTx(ctx, scopedPropertyID, func(tx pgx.Tx) error {
		query := `SELECT ` + refundApprovalCols + ` FROM refund_approval_requests WHERE property_id = $1`
		args := []any{scopedPropertyID}
		if status != "" {
			query += ` AND status = $2`
			args = append(args, status)
		}
		query += ` ORDER BY created_at DESC LIMIT 201`
		rows, queryErr := tx.Query(ctx, query, args...)
		if queryErr != nil {
			return queryErr
		}
		defer rows.Close()
		for rows.Next() {
			req, scanErr := r.scanRow(rows)
			if scanErr != nil {
				return scanErr
			}
			list = append(list, *req)
		}
		return rows.Err()
	})
	return list, err
}

func (r *RefundApprovalRepo) UpdateRequest(ctx context.Context, req *domain.RefundApprovalRequest) error {
	if req == nil {
		return errors.New("refund approval request is required")
	}
	propertyID, err := refundApprovalScope(ctx, req.PropertyID)
	if err != nil {
		return err
	}
	req.UpdatedAt = time.Now().UTC()
	approvalsJSON, err := json.Marshal(req.Approvals)
	if err != nil {
		return err
	}
	return r.withScopedTx(ctx, propertyID, func(tx pgx.Tx) error {
		res, execErr := tx.Exec(ctx, `
			UPDATE refund_approval_requests SET
				amount_paise = $3,
				reason = $4,
				status = $5,
				required_approvals = $6,
				material_hash = $7,
				approvals = $8,
				rejected_by = $9,
				rejected_at = $10,
				rejection_reason = $11,
				refund_id = $12,
				updated_at = $13
			WHERE id = $1 AND property_id = $2`,
			req.ID, propertyID, req.AmountPaise, req.Reason, req.Status, req.RequiredApprovals, req.MaterialHash,
			approvalsJSON, req.RejectedBy, req.RejectedAt, req.RejectionReason, req.RefundID, req.UpdatedAt,
		)
		if execErr != nil {
			return execErr
		}
		if res.RowsAffected() == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
}

// RecordApprovalAtomic executes an approval evaluation inside an atomic database transaction
// with row-level locking (FOR UPDATE) to prevent concurrency races and double dispatches.
func (r *RefundApprovalRepo) RecordApprovalAtomic(
	ctx context.Context,
	requestID uuid.UUID,
	decision domain.RefundApprovalDecision,
	requiredApprovals int,
	onApproved func(tx pgx.Tx, req *domain.RefundApprovalRequest) error,
) (*domain.RefundApprovalRequest, error) {
	propertyID, err := refundApprovalScope(ctx, uuid.Nil)
	if err != nil {
		return nil, err
	}
	var result *domain.RefundApprovalRequest
	err = r.withScopedTx(ctx, propertyID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT `+refundApprovalCols+` FROM refund_approval_requests WHERE id = $1 AND property_id = $2 FOR UPDATE`,
			requestID, propertyID,
		)
		req, scanErr := r.scanRow(row)
		if scanErr != nil {
			return scanErr
		}

		if req.Status == domain.RefundApprovalRejected || req.Status == domain.RefundApprovalCancelled {
			return domain.ErrRefundRequestNotPending
		}
		if req.Status == domain.RefundApprovalApproved || req.Status == domain.RefundApprovalDispatched {
			result = req
			return nil
		}
		if decision.ApproverID == req.RequestedBy {
			return domain.ErrMakerCannotApprove
		}
		if decision.SnapshotHash != req.MaterialHash {
			return domain.ErrStaleApproval
		}
		for _, prev := range req.Approvals {
			if prev.ApproverID == decision.ApproverID {
				return domain.ErrDuplicateApprover
			}
			if prev.SnapshotHash != req.MaterialHash {
				return domain.ErrStaleApproval
			}
		}

		req.Approvals = append(req.Approvals, decision)
		req.UpdatedAt = time.Now().UTC()
		if requiredApprovals > 0 {
			req.RequiredApprovals = requiredApprovals
		}
		if len(req.Approvals) >= req.RequiredApprovals {
			req.Status = domain.RefundApprovalApproved
			if onApproved != nil {
				if err := onApproved(tx, req); err != nil {
					return fmt.Errorf("execute onApproved dispatch: %w", err)
				}
				req.Status = domain.RefundApprovalDispatched
			}
		} else {
			req.Status = domain.RefundApprovalPartiallyApproved
		}
		approvalsJSON, marshalErr := json.Marshal(req.Approvals)
		if marshalErr != nil {
			return marshalErr
		}
		_, updateErr := tx.Exec(ctx, `
			UPDATE refund_approval_requests SET
				status = $3,
				required_approvals = $4,
				approvals = $5,
				refund_id = $6,
				updated_at = $7
			WHERE id = $1 AND property_id = $2`,
			req.ID, propertyID, req.Status, req.RequiredApprovals, approvalsJSON, req.RefundID, req.UpdatedAt,
		)
		if updateErr != nil {
			return fmt.Errorf("update refund approval request: %w", updateErr)
		}
		result = req
		return nil
	})
	return result, err
}

func (r *RefundApprovalRepo) RejectRequestAtomic(
	ctx context.Context,
	requestID uuid.UUID,
	rejecterID uuid.UUID,
	reason string,
) (*domain.RefundApprovalRequest, error) {
	propertyID, err := refundApprovalScope(ctx, uuid.Nil)
	if err != nil {
		return nil, err
	}
	var result *domain.RefundApprovalRequest
	err = r.withScopedTx(ctx, propertyID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx,
			`SELECT `+refundApprovalCols+` FROM refund_approval_requests WHERE id = $1 AND property_id = $2 FOR UPDATE`,
			requestID, propertyID,
		)
		req, scanErr := r.scanRow(row)
		if scanErr != nil {
			return scanErr
		}
		if req.Status == domain.RefundApprovalApproved || req.Status == domain.RefundApprovalDispatched {
			return errors.New("cannot reject an approved or dispatched refund request")
		}
		now := time.Now().UTC()
		req.Status = domain.RefundApprovalRejected
		req.RejectedBy = &rejecterID
		req.RejectedAt = &now
		req.RejectionReason = &reason
		req.UpdatedAt = now
		_, updateErr := tx.Exec(ctx, `
			UPDATE refund_approval_requests SET
				status = $3,
				rejected_by = $4,
				rejected_at = $5,
				rejection_reason = $6,
				updated_at = $7
			WHERE id = $1 AND property_id = $2`,
			req.ID, propertyID, req.Status, req.RejectedBy, req.RejectedAt, req.RejectionReason, req.UpdatedAt,
		)
		if updateErr != nil {
			return updateErr
		}
		result = req
		return nil
	})
	return result, err
}
