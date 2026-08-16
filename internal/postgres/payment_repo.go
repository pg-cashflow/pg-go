package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type PaymentRepo struct{ db DBTX }

func NewPaymentRepo(db DBTX) *PaymentRepo { return &PaymentRepo{db: db} }

func (r *PaymentRepo) WithTx(tx pgx.Tx) *PaymentRepo { return &PaymentRepo{db: tx} }

func (r *PaymentRepo) Create(ctx context.Context, p *domain.Payment) error {
	now := time.Now().UTC()
	if p.MatchedAt.IsZero() {
		p.MatchedAt = now
	}
	p.CreatedAt = now
	return r.db.QueryRow(ctx, `
		INSERT INTO payments (due_id, tenant_id, upi_txn_id, amount, matched_by, recorded_by, matched_at, raw_note, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id`,
		p.DueID, p.TenantID, p.UPITxnID, p.Amount, p.MatchedBy, p.RecordedBy, p.MatchedAt, p.RawNote, p.CreatedAt,
	).Scan(&p.ID)
}

func (r *PaymentRepo) ListByProperty(ctx context.Context, propertyID uuid.UUID, matchedBy *domain.MatchedBy) ([]domain.Payment, error) {
	q := `
		SELECT p.id, p.due_id, p.tenant_id, p.upi_txn_id, p.amount, p.matched_by, p.recorded_by, p.matched_at, p.raw_note, p.created_at
		FROM payments p
		JOIN dues d ON d.id = p.due_id
		WHERE d.property_id=$1`
	args := []any{propertyID}
	if matchedBy != nil {
		q += ` AND p.matched_by=$2`
		args = append(args, *matchedBy)
	}
	q += ` ORDER BY p.matched_at DESC`
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Payment
	for rows.Next() {
		var p domain.Payment
		if err := rows.Scan(&p.ID, &p.DueID, &p.TenantID, &p.UPITxnID, &p.Amount, &p.MatchedBy, &p.RecordedBy, &p.MatchedAt, &p.RawNote, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *PaymentRepo) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.Payment, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, due_id, tenant_id, upi_txn_id, amount, matched_by, recorded_by, matched_at, raw_note, created_at
		FROM payments WHERE tenant_id=$1 ORDER BY matched_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Payment
	for rows.Next() {
		var p domain.Payment
		if err := rows.Scan(&p.ID, &p.DueID, &p.TenantID, &p.UPITxnID, &p.Amount, &p.MatchedBy, &p.RecordedBy, &p.MatchedAt, &p.RawNote, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *PaymentRepo) GetByDueID(ctx context.Context, dueID uuid.UUID) (*domain.Payment, error) {
	var p domain.Payment
	err := r.db.QueryRow(ctx, `
		SELECT id, due_id, tenant_id, upi_txn_id, amount, matched_by, recorded_by, matched_at, raw_note, created_at
		FROM payments WHERE due_id=$1`, dueID).Scan(
		&p.ID, &p.DueID, &p.TenantID, &p.UPITxnID, &p.Amount, &p.MatchedBy, &p.RecordedBy, &p.MatchedAt, &p.RawNote, &p.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *PaymentRepo) GetByUPITxnID(ctx context.Context, txnID string) (*domain.Payment, error) {
	var p domain.Payment
	err := r.db.QueryRow(ctx, `
		SELECT id, due_id, tenant_id, upi_txn_id, amount, matched_by, recorded_by, matched_at, raw_note, created_at
		FROM payments WHERE upi_txn_id=$1`, txnID).Scan(
		&p.ID, &p.DueID, &p.TenantID, &p.UPITxnID, &p.Amount, &p.MatchedBy, &p.RecordedBy, &p.MatchedAt, &p.RawNote, &p.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &p, nil
}
