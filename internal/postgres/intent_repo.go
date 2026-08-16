package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type PaymentIntentRepo struct{ db DBTX }

func NewPaymentIntentRepo(db DBTX) *PaymentIntentRepo { return &PaymentIntentRepo{db: db} }

func (r *PaymentIntentRepo) WithTx(tx pgx.Tx) *PaymentIntentRepo { return &PaymentIntentRepo{db: tx} }

const intentCols = `id, due_id, provider, provider_order_id, payment_session_id, amount_paise, status, expires_at, cf_payment_id, created_at, updated_at`

func scanIntent(row pgx.Row) (*domain.PaymentIntent, error) {
	var p domain.PaymentIntent
	err := row.Scan(&p.ID, &p.DueID, &p.Provider, &p.ProviderOrderID, &p.PaymentSessionID, &p.AmountPaise, &p.Status, &p.ExpiresAt, &p.CFPaymentID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *PaymentIntentRepo) Create(ctx context.Context, p *domain.PaymentIntent) error {
	now := time.Now().UTC()
	p.CreatedAt = now
	p.UpdatedAt = now
	if p.Status == "" {
		p.Status = domain.IntentCreated
	}
	return r.db.QueryRow(ctx, `
		INSERT INTO payment_intents (due_id, provider, provider_order_id, payment_session_id, amount_paise, status, expires_at, cf_payment_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		p.DueID, p.Provider, p.ProviderOrderID, p.PaymentSessionID, p.AmountPaise, p.Status, p.ExpiresAt, p.CFPaymentID, p.CreatedAt, p.UpdatedAt,
	).Scan(&p.ID)
}

func (r *PaymentIntentRepo) GetByOrderID(ctx context.Context, orderID string) (*domain.PaymentIntent, error) {
	return scanIntent(r.db.QueryRow(ctx, `SELECT `+intentCols+` FROM payment_intents WHERE provider_order_id=$1`, orderID))
}

func (r *PaymentIntentRepo) GetByCFPaymentID(ctx context.Context, cfID string) (*domain.PaymentIntent, error) {
	return scanIntent(r.db.QueryRow(ctx, `SELECT `+intentCols+` FROM payment_intents WHERE cf_payment_id=$1`, cfID))
}

func (r *PaymentIntentRepo) LatestOpenForDue(ctx context.Context, dueID uuid.UUID) (*domain.PaymentIntent, error) {
	return scanIntent(r.db.QueryRow(ctx, `
		SELECT `+intentCols+` FROM payment_intents
		WHERE due_id=$1 AND status='created' AND (expires_at IS NULL OR expires_at > NOW())
		ORDER BY created_at DESC LIMIT 1`, dueID))
}

func (r *PaymentIntentRepo) ListStaleCreated(ctx context.Context, olderThan time.Time) ([]domain.PaymentIntent, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+intentCols+` FROM payment_intents
		WHERE status='created' AND created_at < $1
		ORDER BY created_at ASC`, olderThan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PaymentIntent
	for rows.Next() {
		p, err := scanIntent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (r *PaymentIntentRepo) MarkPaid(ctx context.Context, id uuid.UUID, cfPaymentID string) error {
	_, err := r.db.Exec(ctx, `
		UPDATE payment_intents SET status='paid', cf_payment_id=$2, updated_at=NOW() WHERE id=$1`,
		id, cfPaymentID)
	return err
}

func (r *PaymentIntentRepo) Touch(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.Exec(ctx, `UPDATE payment_intents SET updated_at=NOW() WHERE id=$1`, id)
	return err
}

func (r *PaymentIntentRepo) RecencyForDue(ctx context.Context, dueID uuid.UUID) (count int, latest *time.Time, err error) {
	err = r.db.QueryRow(ctx, `
		SELECT COUNT(*), MAX(updated_at) FROM payment_intents WHERE due_id=$1`, dueID).Scan(&count, &latest)
	return count, latest, err
}
