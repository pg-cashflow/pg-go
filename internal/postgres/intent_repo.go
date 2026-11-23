package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/requestscope"
)

type PaymentIntentRepo struct{ db DBTX }

func NewPaymentIntentRepo(db DBTX) *PaymentIntentRepo { return &PaymentIntentRepo{db: db} }

func (r *PaymentIntentRepo) WithTx(tx pgx.Tx) *PaymentIntentRepo { return &PaymentIntentRepo{db: tx} }

const intentCols = `id, due_id, provider, provider_order_id, payment_session_id, amount_paise, status, expires_at, cf_payment_id, created_at, updated_at, idempotency_key`

func scanIntent(row pgx.Row) (*domain.PaymentIntent, error) {
	var p domain.PaymentIntent
	var dueID *uuid.UUID
	var idemKey *uuid.UUID
	err := row.Scan(&p.ID, &dueID, &p.Provider, &p.ProviderOrderID, &p.PaymentSessionID, &p.AmountPaise, &p.Status, &p.ExpiresAt, &p.CFPaymentID, &p.CreatedAt, &p.UpdatedAt, &idemKey)
	if err != nil {
		return nil, err
	}
	if dueID != nil {
		p.DueID = *dueID
	}
	if idemKey != nil {
		p.IdempotencyKey = *idemKey
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
	if p.IdempotencyKey == uuid.Nil {
		p.IdempotencyKey = uuid.New()
	}
	var dueID *uuid.UUID
	if p.DueID != uuid.Nil {
		dueID = &p.DueID
	}
	return r.db.QueryRow(ctx, `
		INSERT INTO payment_intents (due_id, provider, provider_order_id, payment_session_id, amount_paise, status, expires_at, cf_payment_id, created_at, updated_at, idempotency_key)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING id`,
		dueID, p.Provider, p.ProviderOrderID, p.PaymentSessionID, p.AmountPaise, p.Status, p.ExpiresAt, p.CFPaymentID, p.CreatedAt, p.UpdatedAt, p.IdempotencyKey,
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
		WHERE (due_id=$1 OR id IN (SELECT payment_intent_id FROM payment_intent_dues WHERE due_id=$1))
		AND status IN ('created', 'initiating', 'paid') AND (expires_at IS NULL OR expires_at > NOW() - INTERVAL '1 hour')
		ORDER BY created_at DESC LIMIT 1`, dueID))
}

func (r *PaymentIntentRepo) ListStaleCreated(ctx context.Context, olderThan time.Time) ([]domain.PaymentIntent, error) {
	rows, err := r.db.Query(ctx, `
		SELECT `+intentCols+` FROM payment_intents
		WHERE status IN ('created', 'superseded', 'initiating', 'dispatch_unknown')
		  AND (updated_at < $1 OR (created_at < $1 AND updated_at IS NULL))
		  AND (expires_at IS NULL OR expires_at + INTERVAL '2 hours' > NOW())
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

func (r *PaymentIntentRepo) TouchNonTerminalByOrderID(ctx context.Context, orderID string) error {
	_, err := r.db.Exec(ctx, `
		UPDATE payment_intents
		SET updated_at=NOW()
		WHERE provider_order_id=$1
		  AND status NOT IN ('paid', 'refunded', 'failed', 'superseded')`,
		orderID,
	)
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

	if pool := PoolFromDB(r.db); pool != nil {
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

func (r *PaymentIntentRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.PaymentIntent, error) {
	return scanIntent(r.db.QueryRow(ctx, `SELECT `+intentCols+` FROM payment_intents WHERE id=$1`, id))
}

var ErrPaymentIntentQuarantinePending = errors.New("payment checkout blocked: an earlier Cashfree order is still being reconciled")

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
	if len(amounts) != len(dueIDs) {
		return nil, false, false, errors.New("payment intent claim requires one amount per due")
	}
	if tenantID == uuid.Nil || totalAmountPaise <= 0 {
		return nil, false, false, errors.New("payment intent claim requires a tenant and positive total amount")
	}
	scopedPropertyID, scoped := requestscope.PropertyIDFromContext(ctx)
	if !scoped || scopedPropertyID == uuid.Nil {
		return nil, false, false, errors.New("payment intent claim requires a property-scoped request context")
	}
	const maxInt64 = int64(^uint64(0) >> 1)
	seenDueIDs := make(map[uuid.UUID]struct{}, len(dueIDs))
	var validatedTotal int64
	for i, dueID := range dueIDs {
		if dueID == uuid.Nil {
			return nil, false, false, fmt.Errorf("payment intent claim contains an empty due ID at index %d", i)
		}
		if _, duplicate := seenDueIDs[dueID]; duplicate {
			return nil, false, false, fmt.Errorf("payment intent claim contains duplicate due %s", dueID)
		}
		seenDueIDs[dueID] = struct{}{}
		if amounts[i] <= 0 || validatedTotal > maxInt64-amounts[i] {
			return nil, false, false, errors.New("payment intent claim contains an invalid or overflowing due amount")
		}
		validatedTotal += amounts[i]
	}
	if validatedTotal != totalAmountPaise {
		return nil, false, false, fmt.Errorf("payment intent claim total mismatch: amounts sum to %d paise, total is %d paise", validatedTotal, totalAmountPaise)
	}

	var quarantineBlockedAfterCommit error
	execClaim := func(txRepo *PaymentIntentRepo) (*domain.PaymentIntent, bool, bool, error) {
		// 1. Universal lock order: tenants -> dues -> payment_intents.
		// Validate the complete due set while holding the locks. This also proves
		// that every requested due belongs to this tenant and property and that
		// the provider order amount matches the current database payable amounts.
		var tenantStatus domain.TenantStatus
		var tenantPropertyID uuid.UUID
		if err := txRepo.db.QueryRow(ctx, `SELECT status, property_id FROM tenants WHERE id=$1 FOR UPDATE`, tenantID).Scan(&tenantStatus, &tenantPropertyID); err != nil {
			if isNoRows(err) {
				return nil, false, false, errors.New("tenant not found")
			}
			return nil, false, false, fmt.Errorf("lock tenant: %w", err)
		}
		if tenantStatus != domain.TenantStatusActive {
			return nil, false, false, fmt.Errorf("cannot claim payment intent: tenant status is %q", tenantStatus)
		}
		if tenantPropertyID != scopedPropertyID {
			return nil, false, false, errors.New("payment intent claim tenant does not belong to the scoped property")
		}
		rows, err := txRepo.db.Query(ctx, `
			SELECT id, tenant_id, property_id, amount, status
			FROM dues
			WHERE id = ANY($1)
			ORDER BY due_date ASC, id ASC
			FOR UPDATE`, dueIDs)
		if err != nil {
			return nil, false, false, fmt.Errorf("lock dues: %w", err)
		}
		lockedDueAmounts := make(map[uuid.UUID]int64, len(dueIDs))
		for rows.Next() {
			var lockedDueID, lockedTenantID, lockedPropertyID uuid.UUID
			var dueAmount int64
			var dueStatus domain.DueStatus
			if scanErr := rows.Scan(&lockedDueID, &lockedTenantID, &lockedPropertyID, &dueAmount, &dueStatus); scanErr != nil {
				rows.Close()
				return nil, false, false, fmt.Errorf("scan locked due: %w", scanErr)
			}
			if lockedTenantID != tenantID || lockedPropertyID != scopedPropertyID {
				rows.Close()
				return nil, false, false, errors.New("payment intent claim includes a due outside the tenant or property scope")
			}
			if dueStatus != domain.DueStatusPending && dueStatus != domain.DueStatusPartial {
				rows.Close()
				return nil, false, false, fmt.Errorf("payment intent claim due %s is not payable (status %q)", lockedDueID, dueStatus)
			}
			lockedDueAmounts[lockedDueID] = dueAmount
		}
		rowsErr := rows.Err()
		rows.Close()
		if rowsErr != nil {
			return nil, false, false, fmt.Errorf("iterate locked dues: %w", rowsErr)
		}
		if len(lockedDueAmounts) != len(dueIDs) {
			return nil, false, false, errors.New("payment intent claim contains a due that does not exist or is not visible in the scoped property")
		}
		for i, dueID := range dueIDs {
			if lockedDueAmounts[dueID] != amounts[i] {
				return nil, false, false, fmt.Errorf("payment intent claim amount mismatch for due %s: database has %d paise, request has %d paise", dueID, lockedDueAmounts[dueID], amounts[i])
			}
		}

		// A superseded Cashfree order can remain payable at the provider. Do not
		// reuse or create a checkout while any affected due has an unresolved
		// quarantine record. Lock the queue row after tenant and due locks.
		var quarantinedOrderID string
		qErr := txRepo.db.QueryRow(ctx, `
			SELECT provider_order_id
			FROM payment_intent_quarantine
			WHERE status <> 'resolved' AND due_ids && $1::uuid[]
			ORDER BY created_at ASC
			LIMIT 1 FOR UPDATE`, dueIDs,
		).Scan(&quarantinedOrderID)
		if qErr == nil {
			return nil, false, false, fmt.Errorf("%w: provider order %s", ErrPaymentIntentQuarantinePending, quarantinedOrderID)
		}
		if !isNoRows(qErr) {
			return nil, false, false, fmt.Errorf("check payment intent quarantine: %w", qErr)
		}

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
				SELECT pi.id, pi.due_id, pi.provider, pi.provider_order_id, pi.payment_session_id, pi.amount_paise, pi.status, pi.expires_at, pi.cf_payment_id, pi.created_at, pi.updated_at, pi.idempotency_key
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

		// 3a. Check for intent in 'needs_review' - unresolved provider order requires manual review; NO new checkout permitted
		var underReview *domain.PaymentIntent
		row := txRepo.db.QueryRow(ctx, `
			SELECT `+intentCols+` FROM payment_intents
			WHERE status = 'needs_review'
			  AND (
				due_id = ANY($1)
				OR id IN (SELECT payment_intent_id FROM payment_intent_dues WHERE due_id = ANY($1))
			  )
			ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, dueIDs,
		)
		underReview, err = scanIntent(row)
		if err == nil && underReview != nil {
			return nil, false, false, fmt.Errorf("due has payment intent under review (%s): checkout blocked until resolved", underReview.ID)
		} else if err != nil && !isNoRows(err) {
			return nil, false, false, fmt.Errorf("check intent under review: %w", err)
		}

		// 3b. Check for in-flight / unresolved intent (initiating or dispatch_unknown)
		var inFlight *domain.PaymentIntent
		row = txRepo.db.QueryRow(ctx, `
			SELECT `+intentCols+` FROM payment_intents
			WHERE status IN ('initiating', 'dispatch_unknown')
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

		// Also check for active 'created' intent whose expiry is in the future
		var activeCreated *domain.PaymentIntent
		row = txRepo.db.QueryRow(ctx, `
			SELECT `+intentCols+` FROM payment_intents
			WHERE status = 'created'
			  AND (expires_at IS NULL OR expires_at > NOW())
			  AND (
				due_id = ANY($1)
				OR id IN (SELECT payment_intent_id FROM payment_intent_dues WHERE due_id = ANY($1))
			  )
			ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, dueIDs,
		)
		activeCreated, err = scanIntent(row)
		if err == nil && activeCreated != nil {
			if activeCreated.PaymentSessionID == nil || *activeCreated.PaymentSessionID == "" {
				// A created order without a session can still be payable at the
				// provider. Keep the current intent in flight; never create another.
				return activeCreated, false, true, nil
			}
			if activeCreated.ExpiresAt == nil || activeCreated.ExpiresAt.After(time.Now().Add(minRemaining)) {
				return activeCreated, true, false, nil
			}
			// The session exists but has insufficient remaining lifetime. Fall
			// through and supersede it; the DB trigger quarantines the provider order.
		} else if err != nil && !isNoRows(err) {
			return nil, false, false, fmt.Errorf("check active created intent: %w", err)
		}

		// 4. Supersede provider orders that are expired or do not have the
		// required session lifetime. The trigger writes durable quarantine state.
		_, err = txRepo.db.Exec(ctx, `
			UPDATE payment_intents
			SET status='superseded', updated_at=NOW()
			WHERE status = 'created'
			  AND expires_at IS NOT NULL
			  AND expires_at <= NOW() + $2::interval
			  AND (
				due_id = ANY($1)
				OR id IN (SELECT payment_intent_id FROM payment_intent_dues WHERE due_id = ANY($1))
			  )`, dueIDs, fmt.Sprintf("%d seconds", maxInt(0, int(minRemaining.Seconds()))))
		if err != nil {
			return nil, false, false, fmt.Errorf("supersede expiring intents: %w", err)
		}

		// The database trigger writes quarantine rows in this same transaction.
		// Commit those rows, then return a pending-checkout error. Returning an
		// error from this callback would roll back the quarantine record.
		var newlyQuarantinedOrderID string
		qErr = txRepo.db.QueryRow(ctx, `
			SELECT provider_order_id
			FROM payment_intent_quarantine
			WHERE status <> 'resolved' AND due_ids && $1::uuid[]
			ORDER BY created_at ASC
			LIMIT 1 FOR UPDATE`, dueIDs,
		).Scan(&newlyQuarantinedOrderID)
		if qErr == nil {
			quarantineBlockedAfterCommit = fmt.Errorf("%w: provider order %s", ErrPaymentIntentQuarantinePending, newlyQuarantinedOrderID)
			return nil, false, false, nil
		}
		if !isNoRows(qErr) {
			return nil, false, false, fmt.Errorf("verify payment intent quarantine: %w", qErr)
		}

		// 5. Insert new intent in 'initiating' status
		newID := uuid.New()
		idempotencyKey := uuid.New()
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
			IdempotencyKey:  idempotencyKey,
		}
		if parentDueID != nil {
			newIntent.DueID = *parentDueID
		}

		recoverActiveOnConflict := func() (*domain.PaymentIntent, bool, bool, error) {
			r := txRepo.db.QueryRow(ctx, `
				SELECT `+intentCols+` FROM payment_intents
				WHERE status IN ('initiating', 'created', 'dispatch_unknown')
				  AND (
					due_id = ANY($1)
					OR id IN (SELECT payment_intent_id FROM payment_intent_dues WHERE due_id = ANY($1))
				  )
				ORDER BY created_at DESC LIMIT 1`, dueIDs,
			)
			act, aErr := scanIntent(r)
			if aErr == nil && act != nil {
				if act.Status == domain.IntentCreated && act.PaymentSessionID != nil && (act.ExpiresAt == nil || act.ExpiresAt.After(now)) {
					return act, true, false, nil
				}
				return act, false, true, nil
			}
			return nil, false, false, fmt.Errorf("insert conflict recovery: %w", aErr)
		}

		_, err = txRepo.db.Exec(ctx, `
			INSERT INTO payment_intents (id, due_id, provider, provider_order_id, amount_paise, status, created_at, updated_at, idempotency_key)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			newID, parentDueID, "cashfree", orderID, totalAmountPaise, domain.IntentInitiating, now, now, idempotencyKey,
		)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return recoverActiveOnConflict()
			}
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
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23505" {
					return recoverActiveOnConflict()
				}
				return nil, false, false, fmt.Errorf("insert intent due: %w", err)
			}
		}

		return newIntent, false, false, nil
	}

	// ClaimInitiatingIntent owns its transaction. An outer transaction cannot
	// commit the quarantine row and then return the blocked-checkout result.
	if _, nested := TxFromContext(ctx); nested {
		return nil, false, false, errors.New("payment intent claim must own its transaction")
	}
	pool := PoolFromDB(r.db)
	if pool == nil {
		return nil, false, false, errors.New("payment intent claim requires a pool-backed database handle")
	}
	var intent *domain.PaymentIntent
	var reused, inFlight bool
	err = WithinTx(ctx, pool, func(tx pgx.Tx) error {
		var txErr error
		intent, reused, inFlight, txErr = execClaim(r.WithTx(tx))
		return txErr
	})
	if err != nil {
		return nil, false, false, err
	}
	if quarantineBlockedAfterCommit != nil {
		return nil, false, false, quarantineBlockedAfterCommit
	}
	return intent, reused, inFlight, nil
}

func (r *PaymentIntentRepo) TransitionCreated(ctx context.Context, intentID uuid.UUID, sessionID string, expiresAt *time.Time) error {
	_, err := r.db.Exec(ctx, `
		UPDATE payment_intents
		SET status='created', payment_session_id=$2, expires_at=$3, updated_at=NOW()
		WHERE id=$1 AND status IN ('initiating', 'dispatch_unknown')`,
		intentID, sessionID, expiresAt,
	)
	return err
}

func (r *PaymentIntentRepo) TransitionFailed(ctx context.Context, intentID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		UPDATE payment_intents
		SET status='failed', updated_at=NOW()
		WHERE id=$1 AND status IN ('initiating', 'dispatch_unknown')`,
		intentID,
	)
	return err
}

func (r *PaymentIntentRepo) TransitionDispatchUnknown(ctx context.Context, intentID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		UPDATE payment_intents
		SET status='dispatch_unknown', updated_at=NOW()
		WHERE id=$1 AND status IN ('initiating', 'created')`,
		intentID,
	)
	return err
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func isNoRows(err error) bool {
	return errors.Is(err, pgx.ErrNoRows)
}
