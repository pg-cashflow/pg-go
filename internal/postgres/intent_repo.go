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

func (r *PaymentIntentRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.PaymentIntent, error) {
	return scanIntent(r.db.QueryRow(ctx, `SELECT `+intentCols+` FROM payment_intents WHERE id=$1`, id))
}

func (r *PaymentIntentRepo) ClaimInitiatingIntent(
	ctx context.Context,
	tenantID uuid.UUID,
	dueIDs []uuid.UUID,
	amounts []int64,
	totalAmountPaise int64,
	orderID string,
	minRemaining time.Duration,
) (claimed *domain.PaymentIntent, isReused bool, isInFlight bool, err error) {
	if len(dueIDs) == 0 {
		return nil, false, false, errors.New("dueIDs required")
	}

	execClaim := func(txRepo *PaymentIntentRepo) (*domain.PaymentIntent, bool, bool, error) {
		// 1. Universal lock order: tenants -> dues -> payment_intents
		var dummy uuid.UUID
		if err := txRepo.db.QueryRow(ctx, `SELECT id FROM tenants WHERE id=$1 FOR UPDATE`, tenantID).Scan(&dummy); err != nil && !isNoRows(err) {
			return nil, false, false, fmt.Errorf("lock tenant: %w", err)
		}
		rows, err := txRepo.db.Query(ctx, `SELECT id FROM dues WHERE id = ANY($1) ORDER BY due_date ASC, id ASC FOR UPDATE`, dueIDs)
		if err != nil {
			return nil, false, false, fmt.Errorf("lock dues: %w", err)
		}
		rows.Close()

		// 2. Check for active reusable intent (status='created' with valid session and > minRemaining)
		var existing *domain.PaymentIntent
		if len(dueIDs) == 1 {
			row := txRepo.db.QueryRow(ctx, `
				SELECT `+intentCols+` FROM payment_intents
				WHERE (due_id=$1 OR id IN (SELECT payment_intent_id FROM payment_intent_dues WHERE due_id=$1))
				  AND status='created'
				  AND amount_paise=$2
				  AND payment_session_id IS NOT NULL
				  AND (expires_at IS NULL OR expires_at > NOW() + $3::interval)
				ORDER BY created_at DESC LIMIT 1 FOR UPDATE`,
				dueIDs[0], totalAmountPaise, fmt.Sprintf("%d seconds", int(minRemaining.Seconds())),
			)
			existing, err = scanIntent(row)
		} else {
			row := txRepo.db.QueryRow(ctx, `
				SELECT pi.id, pi.due_id, pi.provider, pi.provider_order_id, pi.payment_session_id, pi.amount_paise, pi.status, pi.expires_at, pi.cf_payment_id, pi.created_at, pi.updated_at
				FROM payment_intents pi
				JOIN (
					SELECT payment_intent_id, array_agg(due_id ORDER BY due_id) as intent_dues
					FROM payment_intent_dues
					GROUP BY payment_intent_id
				) pid ON pid.payment_intent_id = pi.id
				WHERE pi.status='created'
				  AND pi.amount_paise = $1
				  AND pi.payment_session_id IS NOT NULL
				  AND (pi.expires_at IS NULL OR pi.expires_at > NOW() + $2::interval)
				  AND pid.intent_dues = (SELECT array_agg(u ORDER BY u) FROM unnest($3::uuid[]) u)
				ORDER BY pi.created_at DESC
				LIMIT 1 FOR UPDATE OF pi`,
				totalAmountPaise, fmt.Sprintf("%d seconds", int(minRemaining.Seconds())), dueIDs,
			)
			existing, err = scanIntent(row)
		}
		if err == nil && existing != nil && existing.PaymentSessionID != nil {
			return existing, true, false, nil
		} else if err != nil && !isNoRows(err) {
			return nil, false, false, fmt.Errorf("check reusable intent: %w", err)
		}

		// 3. Check for in-flight recent initiating intent (within 30s)
		var inFlight *domain.PaymentIntent
		row := txRepo.db.QueryRow(ctx, `
			SELECT `+intentCols+` FROM payment_intents
			WHERE status='initiating'
			  AND created_at > NOW() - INTERVAL '30 seconds'
			  AND (
				due_id = ANY($1)
				OR id IN (SELECT payment_intent_id FROM payment_intent_dues WHERE due_id = ANY($1))
			  )
			ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, dueIDs,
		)
		inFlight, err = scanIntent(row)
		if err == nil && inFlight != nil {
			return inFlight, false, true, nil
		} else if err != nil && !isNoRows(err) {
			return nil, false, false, fmt.Errorf("check in-flight intent: %w", err)
		}

		// 4. Supersede older open intents for these dues
		_, err = txRepo.db.Exec(ctx, `
			UPDATE payment_intents
			SET status='superseded', updated_at=NOW()
			WHERE status IN ('initiating', 'created') AND (
				due_id = ANY($1)
				OR id IN (SELECT payment_intent_id FROM payment_intent_dues WHERE due_id = ANY($1))
			)`, dueIDs)
		if err != nil {
			return nil, false, false, fmt.Errorf("supersede open intents: %w", err)
		}

		// 5. Insert new intent in 'initiating' status
		newID := uuid.New()
		now := time.Now().UTC()
		var parentDueID *uuid.UUID
		if len(dueIDs) == 1 {
			parentDueID = &dueIDs[0]
		}
		newIntent := &domain.PaymentIntent{
			ID:              newID,
			Provider:        "cashfree",
			ProviderOrderID: orderID,
			AmountPaise:     totalAmountPaise,
			Status:          domain.IntentInitiating,
			CreatedAt:       now,
			UpdatedAt:       now,
		}
		if parentDueID != nil {
			newIntent.DueID = *parentDueID
		}

		_, err = txRepo.db.Exec(ctx, `
			INSERT INTO payment_intents (id, due_id, provider, provider_order_id, amount_paise, status, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			newID, parentDueID, "cashfree", orderID, totalAmountPaise, domain.IntentInitiating, now, now,
		)
		if err != nil {
			return nil, false, false, fmt.Errorf("insert initiating intent: %w", err)
		}

		for i, did := range dueIDs {
			amt := totalAmountPaise
			if i < len(amounts) && amounts[i] > 0 {
				amt = amounts[i]
			}
			_, err = txRepo.db.Exec(ctx, `
				INSERT INTO payment_intent_dues (payment_intent_id, due_id, amount_paise, status, created_at)
				VALUES ($1, $2, $3, $4, $5)
				ON CONFLICT (payment_intent_id, due_id) DO UPDATE SET amount_paise = EXCLUDED.amount_paise, status = EXCLUDED.status`,
				newID, did, amt, domain.IntentInitiating, now,
			)
			if err != nil {
				return nil, false, false, fmt.Errorf("insert intent due: %w", err)
			}
		}

		return newIntent, false, false, nil
	}

	if pool, ok := r.db.(*pgxpool.Pool); ok {
		var intent *domain.PaymentIntent
		var reused, inFlight bool
		err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			var txErr error
			intent, reused, inFlight, txErr = execClaim(r.WithTx(tx))
			return txErr
		})
		if err != nil {
			return nil, false, false, err
		}
		return intent, reused, inFlight, nil
	}

	return execClaim(r)
}

func (r *PaymentIntentRepo) TransitionCreated(ctx context.Context, intentID uuid.UUID, sessionID string, expiresAt *time.Time) error {
	_, err := r.db.Exec(ctx, `
		UPDATE payment_intents
		SET status='created', payment_session_id=$2, expires_at=$3, updated_at=NOW()
		WHERE id=$1 AND status='initiating'`,
		intentID, sessionID, expiresAt,
	)
	return err
}

func (r *PaymentIntentRepo) TransitionFailed(ctx context.Context, intentID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		UPDATE payment_intents
		SET status='failed', updated_at=NOW()
		WHERE id=$1 AND status='initiating'`,
		intentID,
	)
	return err
}

func (r *PaymentIntentRepo) GetReusableIntentUnderLock(ctx context.Context, tenantID, dueID uuid.UUID, minRemaining time.Duration) (*domain.PaymentIntent, error) {
	query := func(repo *PaymentIntentRepo) (*domain.PaymentIntent, error) {
		// Top-down lock order: tenants -> dues -> payment_intents
		var dummy uuid.UUID
		if err := repo.db.QueryRow(ctx, `SELECT id FROM tenants WHERE id=$1 FOR UPDATE`, tenantID).Scan(&dummy); err != nil && !isNoRows(err) {
			return nil, err
		}
		if err := repo.db.QueryRow(ctx, `SELECT id FROM dues WHERE id=$1 FOR UPDATE`, dueID).Scan(&dummy); err != nil && !isNoRows(err) {
			return nil, err
		}
		// Under-lock inspection for active unexpired intent with >= minRemaining
		row := repo.db.QueryRow(ctx, `
			SELECT `+intentCols+` FROM payment_intents
			WHERE due_id=$1 AND status='created' AND (expires_at IS NULL OR expires_at > NOW() + $2::interval)
			ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, dueID, fmt.Sprintf("%d seconds", int(minRemaining.Seconds())))
		return scanIntent(row)
	}

	if pool, ok := r.db.(*pgxpool.Pool); ok {
		var out *domain.PaymentIntent
		err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			var qErr error
			out, qErr = query(r.WithTx(tx))
			return qErr
		})
		return out, err
	}
	return query(r)
}

func (r *PaymentIntentRepo) GetReusableMultiDueIntentUnderLock(ctx context.Context, tenantID uuid.UUID, dueIDs []uuid.UUID, totalAmountPaise int64, minRemaining time.Duration) (*domain.PaymentIntent, error) {
	if len(dueIDs) == 0 {
		return nil, errors.New("dueIDs required")
	}

	query := func(repo *PaymentIntentRepo) (*domain.PaymentIntent, error) {
		// Universal lock order: tenants -> dues -> payment_intents
		var dummy uuid.UUID
		if err := repo.db.QueryRow(ctx, `SELECT id FROM tenants WHERE id=$1 FOR UPDATE`, tenantID).Scan(&dummy); err != nil && !isNoRows(err) {
			return nil, err
		}
		rows, err := repo.db.Query(ctx, `SELECT id FROM dues WHERE id = ANY($1) ORDER BY due_date ASC, id ASC FOR UPDATE`, dueIDs)
		if err != nil {
			return nil, err
		}
		rows.Close()

		if len(dueIDs) == 1 {
			row := repo.db.QueryRow(ctx, `
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

		row := repo.db.QueryRow(ctx, `
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

	if pool, ok := r.db.(*pgxpool.Pool); ok {
		var out *domain.PaymentIntent
		err := WithinTx(ctx, pool, func(tx pgx.Tx) error {
			var qErr error
			out, qErr = query(r.WithTx(tx))
			return qErr
		})
		return out, err
	}
	return query(r)
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}



