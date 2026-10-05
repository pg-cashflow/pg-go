package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type PaymentIntentRepo struct{ db DBTX }

func NewPaymentIntentRepo(db DBTX) *PaymentIntentRepo { return &PaymentIntentRepo{db: db} }

func (r *PaymentIntentRepo) WithTx(tx pgx.Tx) *PaymentIntentRepo { return &PaymentIntentRepo{db: tx} }

const intentCols = `id, due_id, provider, provider_order_id, payment_session_id, amount_paise, status, expires_at, cf_payment_id, created_at, updated_at`

func scanIntent(row pgx.Row) (*domain.PaymentIntent, error) {
	var p domain.PaymentIntent
	var dueID *uuid.UUID
	err := row.Scan(&p.ID, &dueID, &p.Provider, &p.ProviderOrderID, &p.PaymentSessionID, &p.AmountPaise, &p.Status, &p.ExpiresAt, &p.CFPaymentID, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if dueID != nil {
		p.DueID = *dueID
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
	var dueID *uuid.UUID
	if p.DueID != uuid.Nil {
		dueID = &p.DueID
	}
	return r.db.QueryRow(ctx, `
		INSERT INTO payment_intents (due_id, provider, provider_order_id, payment_session_id, amount_paise, status, expires_at, cf_payment_id, created_at, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		dueID, p.Provider, p.ProviderOrderID, p.PaymentSessionID, p.AmountPaise, p.Status, p.ExpiresAt, p.CFPaymentID, p.CreatedAt, p.UpdatedAt,
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
		WHERE status IN ('created', 'superseded') AND created_at < $1 AND (expires_at IS NULL OR expires_at + INTERVAL '2 hours' > NOW())
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

func (r *PaymentIntentRepo) CreateWithDues(ctx context.Context, p *domain.PaymentIntent, dueIDs []uuid.UUID, amounts []int64) error {
	insertAll := func(repo *PaymentIntentRepo) error {
		if err := repo.Create(ctx, p); err != nil {
			return err
		}
		for i, did := range dueIDs {
			amt := int64(p.AmountPaise)
			if i < len(amounts) && amounts[i] > 0 {
				amt = amounts[i]
			}
			status := p.Status
			if status == "" {
				status = domain.IntentCreated
			}
			_, err := repo.db.Exec(ctx, `
				INSERT INTO payment_intent_dues (payment_intent_id, due_id, amount_paise, status)
				VALUES ($1, $2, $3, $4)
				ON CONFLICT (payment_intent_id, due_id) DO UPDATE SET amount_paise = EXCLUDED.amount_paise, status = EXCLUDED.status`,
				p.ID, did, amt, status)
			if err != nil {
				return err
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

func (r *PaymentIntentRepo) GetDuesSnapshot(ctx context.Context, intentID uuid.UUID) ([]domain.PaymentIntentDue, error) {
	rows, err := r.db.Query(ctx, `
		SELECT pid.id, pid.payment_intent_id, pid.due_id, pid.amount_paise, pid.status, pid.created_at
		FROM payment_intent_dues pid
		JOIN dues d ON d.id = pid.due_id
		WHERE pid.payment_intent_id = $1
		ORDER BY d.due_date ASC, d.id ASC`, intentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.PaymentIntentDue
	for rows.Next() {
		var d domain.PaymentIntentDue
		if err := rows.Scan(&d.ID, &d.PaymentIntentID, &d.DueID, &d.AmountPaise, &d.Status, &d.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (r *PaymentIntentRepo) SupersedeOpenIntentsForDue(ctx context.Context, dueID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		UPDATE payment_intents
		SET status='superseded', updated_at=NOW()
		WHERE due_id=$1 AND status='created'`, dueID)
	return err
}

func (r *PaymentIntentRepo) SupersedeOpenIntentsForDues(ctx context.Context, dueIDs []uuid.UUID) error {
	if len(dueIDs) == 0 {
		return nil
	}
	_, err := r.db.Exec(ctx, `
		UPDATE payment_intents
		SET status='superseded', updated_at=NOW()
		WHERE status IN ('initiating', 'created') AND (
			due_id = ANY($1)
			OR id IN (SELECT payment_intent_id FROM payment_intent_dues WHERE due_id = ANY($1))
		)`, dueIDs)
	return err
}

func (r *PaymentIntentRepo) GetReusableIntentUnderLock(ctx context.Context, tenantID, dueID uuid.UUID, minRemaining time.Duration) (*domain.PaymentIntent, error) {
	// Top-down lock order: tenants -> dues -> payment_intents
	var dummy uuid.UUID
	if err := r.db.QueryRow(ctx, `SELECT id FROM tenants WHERE id=$1 FOR UPDATE`, tenantID).Scan(&dummy); err != nil && !isNoRows(err) {
		return nil, err
	}
	if err := r.db.QueryRow(ctx, `SELECT id FROM dues WHERE id=$1 FOR UPDATE`, dueID).Scan(&dummy); err != nil && !isNoRows(err) {
		return nil, err
	}
	// Under-lock inspection for active unexpired intent with >= minRemaining
	row := r.db.QueryRow(ctx, `
		SELECT `+intentCols+` FROM payment_intents
		WHERE due_id=$1 AND status='created' AND (expires_at IS NULL OR expires_at > NOW() + $2::interval)
		ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, dueID, fmt.Sprintf("%d seconds", int(minRemaining.Seconds())))
	return scanIntent(row)
}

func (r *PaymentIntentRepo) GetReusableMultiDueIntentUnderLock(ctx context.Context, tenantID uuid.UUID, dueIDs []uuid.UUID, totalAmountPaise int64, minRemaining time.Duration) (*domain.PaymentIntent, error) {
	if len(dueIDs) == 0 {
		return nil, errors.New("dueIDs required")
	}
	// Universal lock order: tenants -> dues -> payment_intents
	var dummy uuid.UUID
	if err := r.db.QueryRow(ctx, `SELECT id FROM tenants WHERE id=$1 FOR UPDATE`, tenantID).Scan(&dummy); err != nil && !isNoRows(err) {
		return nil, err
	}
	rows, err := r.db.Query(ctx, `SELECT id FROM dues WHERE id = ANY($1) ORDER BY due_date ASC, id ASC FOR UPDATE`, dueIDs)
	if err != nil {
		return nil, err
	}
	rows.Close()

	if len(dueIDs) == 1 {
		row := r.db.QueryRow(ctx, `
			SELECT `+intentCols+` FROM payment_intents
			WHERE (due_id=$1 OR id IN (SELECT payment_intent_id FROM payment_intent_dues WHERE due_id=$1))
			  AND status='created'
			  AND amount_paise=$2
			  AND (expires_at IS NULL OR expires_at > NOW() + $3::interval)
			ORDER BY created_at DESC LIMIT 1 FOR UPDATE`,
			dueIDs[0], totalAmountPaise, fmt.Sprintf("%d seconds", int(minRemaining.Seconds())),
		)
		return scanIntent(row)
	}

	row := r.db.QueryRow(ctx, `
		SELECT pi.id, pi.due_id, pi.provider, pi.provider_order_id, pi.payment_session_id, pi.amount_paise, pi.status, pi.expires_at, pi.cf_payment_id, pi.created_at, pi.updated_at
		FROM payment_intents pi
		JOIN (
			SELECT payment_intent_id, array_agg(due_id ORDER BY due_id) as intent_dues
			FROM payment_intent_dues
			GROUP BY payment_intent_id
		) pid ON pid.payment_intent_id = pi.id
		WHERE pi.status='created'
		  AND pi.amount_paise = $1
		  AND (pi.expires_at IS NULL OR pi.expires_at > NOW() + $2::interval)
		  AND pid.intent_dues = (SELECT array_agg(u ORDER BY u) FROM unnest($3::uuid[]) u)
		ORDER BY pi.created_at DESC
		LIMIT 1 FOR UPDATE OF pi`,
		totalAmountPaise, fmt.Sprintf("%d seconds", int(minRemaining.Seconds())), dueIDs,
	)
	return scanIntent(row)
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}


