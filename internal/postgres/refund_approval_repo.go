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

type RefundApprovalRepo struct {
	pool *pgxpool.Pool
}

func NewRefundApprovalRepo(pool *pgxpool.Pool) *RefundApprovalRepo {
	return &RefundApprovalRepo{pool: pool}
}

func (r *RefundApprovalRepo) CreateRequest(ctx context.Context, req *domain.RefundApprovalRequest) error {
	if r.pool == nil {
		return errors.New("database pool unavailable")
	}
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

	return WithinTx(ctx, r.pool, func(tx pgx.Tx) error {
		if req.RequestedByRole == string(domain.RoleManager) || req.RequestedByRole == "manager" {
			if err := AssertCapabilityTx(ctx, tx, req.PropertyID, req.RequestedBy, req.RequestedByRole, "refunds.issue"); err != nil {
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
	if r.pool == nil {
		return nil, errors.New("database pool unavailable")
	}
	row := r.pool.QueryRow(ctx, `SELECT `+refundApprovalCols+` FROM refund_approval_requests WHERE id = $1`, id)
	return r.scanRow(row)
}

func (r *RefundApprovalRepo) GetRequestByPaymentAndIdempotency(ctx context.Context, paymentID uuid.UUID, idempotencyKey string) (*domain.RefundApprovalRequest, error) {
	if r.pool == nil {
		return nil, errors.New("database pool unavailable")
	}
	row := r.pool.QueryRow(ctx, `SELECT `+refundApprovalCols+` FROM refund_approval_requests WHERE payment_id = $1 AND idempotency_key = $2`, paymentID, idempotencyKey)
	return r.scanRow(row)
}

func (r *RefundApprovalRepo) ListRequestsByPayment(ctx context.Context, paymentID uuid.UUID) ([]domain.RefundApprovalRequest, error) {
	if r.pool == nil {
		return nil, errors.New("database pool unavailable")
	}
	rows, err := r.pool.Query(ctx, `SELECT `+refundApprovalCols+` FROM refund_approval_requests WHERE payment_id = $1 ORDER BY created_at DESC`, paymentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []domain.RefundApprovalRequest
	for rows.Next() {
		req, err := r.scanRow(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *req)
	}
	return list, rows.Err()
}

func (r *RefundApprovalRepo) ListRequestsByProperty(ctx context.Context, propertyID uuid.UUID, status string) ([]domain.RefundApprovalRequest, error) {
	if r.pool == nil {
		return nil, errors.New("database pool unavailable")
	}
	var rows pgx.Rows
	var err error
	if status != "" {
		rows, err = r.pool.Query(ctx, `SELECT `+refundApprovalCols+` FROM refund_approval_requests WHERE property_id = $1 AND status = $2 ORDER BY created_at DESC`, propertyID, status)
	} else {
		rows, err = r.pool.Query(ctx, `SELECT `+refundApprovalCols+` FROM refund_approval_requests WHERE property_id = $1 ORDER BY created_at DESC`, propertyID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var list []domain.RefundApprovalRequest
	for rows.Next() {
		req, err := r.scanRow(rows)
		if err != nil {
			return nil, err
		}
		list = append(list, *req)
	}
	return list, rows.Err()
}

func (r *RefundApprovalRepo) UpdateRequest(ctx context.Context, req *domain.RefundApprovalRequest) error {
	if r.pool == nil {
		return errors.New("database pool unavailable")
	}
	req.UpdatedAt = time.Now().UTC()
	approvalsJSON, err := json.Marshal(req.Approvals)
	if err != nil {
		approvalsJSON = []byte("[]")
	}

	res, err := r.pool.Exec(ctx, `
		UPDATE refund_approval_requests SET
			amount_paise = $2,
			reason = $3,
			status = $4,
			required_approvals = $5,
			material_hash = $6,
			approvals = $7,
			rejected_by = $8,
			rejected_at = $9,
			rejection_reason = $10,
			refund_id = $11,
			updated_at = $12
		WHERE id = $1`,
		req.ID, req.AmountPaise, req.Reason, req.Status, req.RequiredApprovals, req.MaterialHash,
		approvalsJSON, req.RejectedBy, req.RejectedAt, req.RejectionReason, req.RefundID, req.UpdatedAt,
	)
	if err != nil {
		return err
	}
	if res.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
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
	if r.pool == nil {
		return nil, errors.New("database pool unavailable")
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the request row exclusively to serialize approvals
	row := tx.QueryRow(ctx, `SELECT `+refundApprovalCols+` FROM refund_approval_requests WHERE id = $1 FOR UPDATE`, requestID)
	req, err := r.scanRow(row)
	if err != nil {
		return nil, err
	}

	// Terminal or invalid statuses
	if req.Status == domain.RefundApprovalRejected || req.Status == domain.RefundApprovalCancelled {
		return nil, domain.ErrRefundRequestNotPending
	}

	// Idempotent return if already approved/dispatched
	if req.Status == domain.RefundApprovalApproved || req.Status == domain.RefundApprovalDispatched {
		// Verify if this approver already approved
		for _, a := range req.Approvals {
			if a.ApproverID == decision.ApproverID {
				_ = tx.Commit(ctx)
				return req, nil
			}
		}
		_ = tx.Commit(ctx)
		return req, nil
	}

	// 1. Prohibit self-approval (Maker cannot be Checker)
	if decision.ApproverID == req.RequestedBy {
		return nil, domain.ErrMakerCannotApprove
	}

	// 2. Verify approval integrity (SnapshotHash must match current MaterialHash)
	if decision.SnapshotHash != req.MaterialHash {
		return nil, domain.ErrStaleApproval
	}

	// 3. Prohibit one owner from satisfying both approvals
	for _, prev := range req.Approvals {
		if prev.ApproverID == decision.ApproverID {
			return nil, domain.ErrDuplicateApprover
		}
		// If any previous approval had a different snapshot hash, it is stale
		if prev.SnapshotHash != req.MaterialHash {
			return nil, domain.ErrStaleApproval
		}
	}

	// Record new decision
	req.Approvals = append(req.Approvals, decision)
	req.UpdatedAt = time.Now().UTC()

	// Update required approvals if caller passed a specific requirement > 0
	if requiredApprovals > 0 {
		req.RequiredApprovals = requiredApprovals
	}

	// Check if approval threshold reached
	if len(req.Approvals) >= req.RequiredApprovals {
		req.Status = domain.RefundApprovalApproved

		// Execute transactional callback (e.g. outbox dispatch)
		if onApproved != nil {
			if err := onApproved(tx, req); err != nil {
				return nil, fmt.Errorf("execute onApproved dispatch: %w", err)
			}
			req.Status = domain.RefundApprovalDispatched
		}
	} else {
		req.Status = domain.RefundApprovalPartiallyApproved
	}

	approvalsJSON, err := json.Marshal(req.Approvals)
	if err != nil {
		return nil, err
	}

	_, err = tx.Exec(ctx, `
		UPDATE refund_approval_requests SET
			status = $2,
			required_approvals = $3,
			approvals = $4,
			refund_id = $5,
			updated_at = $6
		WHERE id = $1`,
		req.ID, req.Status, req.RequiredApprovals, approvalsJSON, req.RefundID, req.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("update refund approval request: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit tx: %w", err)
	}

	return req, nil
}

func (r *RefundApprovalRepo) RejectRequestAtomic(
	ctx context.Context,
	requestID uuid.UUID,
	rejecterID uuid.UUID,
	reason string,
) (*domain.RefundApprovalRequest, error) {
	if r.pool == nil {
		return nil, errors.New("database pool unavailable")
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	row := tx.QueryRow(ctx, `SELECT `+refundApprovalCols+` FROM refund_approval_requests WHERE id = $1 FOR UPDATE`, requestID)
	req, err := r.scanRow(row)
	if err != nil {
		return nil, err
	}

	if req.Status == domain.RefundApprovalApproved || req.Status == domain.RefundApprovalDispatched {
		return nil, errors.New("cannot reject an approved or dispatched refund request")
	}

	now := time.Now().UTC()
	req.Status = domain.RefundApprovalRejected
	req.RejectedBy = &rejecterID
	req.RejectedAt = &now
	req.RejectionReason = &reason
	req.UpdatedAt = now

	_, err = tx.Exec(ctx, `
		UPDATE refund_approval_requests SET
			status = $2,
			rejected_by = $3,
			rejected_at = $4,
			rejection_reason = $5,
			updated_at = $6
		WHERE id = $1`,
		req.ID, req.Status, req.RejectedBy, req.RejectedAt, req.RejectionReason, req.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	return req, nil
}
