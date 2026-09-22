package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type PaymentReportRepo struct{ db DBTX }

func NewPaymentReportRepo(db DBTX) *PaymentReportRepo { return &PaymentReportRepo{db: db} }

func (r *PaymentReportRepo) WithTx(tx pgx.Tx) *PaymentReportRepo { return &PaymentReportRepo{db: tx} }

const reportCols = `id, due_id, tenant_id, property_id, upi_txn_id, amount, (image_bytes IS NOT NULL), status, reported_by, reviewed_by, reviewed_at, note, created_at, image_hash, is_duplicate, ocr_amount, ocr_utr, ocr_txn_date, ocr_confidence`

func scanReport(row pgx.Row) (*domain.PaymentReport, error) {
	var p domain.PaymentReport
	err := row.Scan(
		&p.ID, &p.DueID, &p.TenantID, &p.PropertyID, &p.UPITxnID, &p.Amount,
		&p.HasImage, &p.Status, &p.ReportedBy, &p.ReviewedBy, &p.ReviewedAt,
		&p.Note, &p.CreatedAt, &p.ImageHash, &p.IsDuplicate,
		&p.OCRAmount, &p.OCRUTR, &p.OCRTxnDate, &p.OCRConfidence,
	)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *PaymentReportRepo) Create(ctx context.Context, p *domain.PaymentReport) error {
	p.CreatedAt = time.Now().UTC()
	if p.Status == "" {
		p.Status = domain.ReportPendingReview
	}
	return r.db.QueryRow(ctx, `
		INSERT INTO payment_reports (
			due_id, tenant_id, property_id, upi_txn_id, amount, image_bytes, status, reported_by, note, created_at,
			image_hash, is_duplicate, ocr_amount, ocr_utr, ocr_txn_date, ocr_confidence
		)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16) RETURNING id`,
		p.DueID, p.TenantID, p.PropertyID, p.UPITxnID, p.Amount, p.ImageBytes, p.Status, p.ReportedBy, p.Note, p.CreatedAt,
		p.ImageHash, p.IsDuplicate, p.OCRAmount, p.OCRUTR, p.OCRTxnDate, p.OCRConfidence,
	).Scan(&p.ID)
}

func (r *PaymentReportRepo) HasImageWithHash(ctx context.Context, propertyID uuid.UUID, hash string) (bool, error) {
	if hash == "" {
		return false, nil
	}
	var exists bool
	err := r.db.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM payment_reports WHERE property_id=$1 AND image_hash=$2)`, propertyID, hash).Scan(&exists)
	return exists, err
}

func (r *PaymentReportRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.PaymentReport, error) {
	return scanReport(r.db.QueryRow(ctx, `SELECT `+reportCols+` FROM payment_reports WHERE id=$1`, id))
}

func (r *PaymentReportRepo) GetByUPITxnID(ctx context.Context, txnID string) (*domain.PaymentReport, error) {
	return scanReport(r.db.QueryRow(ctx, `SELECT `+reportCols+` FROM payment_reports WHERE upi_txn_id=$1`, txnID))
}

func (r *PaymentReportRepo) ListByProperty(ctx context.Context, propertyID uuid.UUID, status *domain.PaymentReportStatus) ([]domain.PaymentReport, error) {
	q := `SELECT ` + reportCols + ` FROM payment_reports WHERE property_id=$1`
	args := []any{propertyID}
	if status != nil {
		q += ` AND status=$2`
		args = append(args, *status)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PaymentReport
	for rows.Next() {
		p, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (r *PaymentReportRepo) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]domain.PaymentReport, error) {
	rows, err := r.db.Query(ctx, `SELECT `+reportCols+` FROM payment_reports WHERE tenant_id=$1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PaymentReport
	for rows.Next() {
		p, err := scanReport(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (r *PaymentReportRepo) UpdateReview(ctx context.Context, p *domain.PaymentReport) error {
	_, err := r.db.Exec(ctx, `
		UPDATE payment_reports SET status=$2, reviewed_by=$3, reviewed_at=$4, note=$5 WHERE id=$1`,
		p.ID, p.Status, p.ReviewedBy, p.ReviewedAt, p.Note,
	)
	return err
}

func (r *PaymentReportRepo) PurgeExpiredImages(ctx context.Context, olderThan time.Duration) (int64, error) {
	if olderThan <= 0 {
		olderThan = 30 * 24 * time.Hour
	}
	cutoff := time.Now().UTC().Add(-olderThan)
	tag, err := r.db.Exec(ctx, `
		UPDATE payment_reports SET image_bytes=NULL
		WHERE image_bytes IS NOT NULL AND created_at < $1`,
		cutoff,
	)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
