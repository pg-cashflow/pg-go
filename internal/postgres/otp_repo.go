package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrOTPAlreadyUsed    = domain.ErrOTPAlreadyUsed
	ErrTokenAlreadyUsed  = domain.ErrTokenAlreadyUsed
	ErrOTPLocked         = errors.New("postgres: otp locked")
	ErrPushEndpointOwned = errors.New("postgres: push endpoint is already registered to a different tenant")
	ErrRateLimited       = errors.New("postgres: otp rate limited")
)

// RateLimitError conveys the remaining duration until retry is permitted.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("postgres: otp rate limited (retry after %v)", e.RetryAfter)
}

func (e *RateLimitError) Is(target error) bool {
	return target == ErrRateLimited
}

type OTPRepo struct{ pool *pgxpool.Pool }

func NewOTPRepo(pool *pgxpool.Pool) *OTPRepo { return &OTPRepo{pool: pool} }

type OTPRequest struct {
	ID          uuid.UUID
	Phone       string
	OTPHash     string
	Attempts    int16
	ExpiresAt   time.Time
	Used        bool
	Purpose     string
	BatchID     *uuid.UUID
	Channel     string
	Destination string
	CreatedAt   time.Time
}

func (r *OTPRepo) Create(ctx context.Context, req *OTPRequest) error {
	req.CreatedAt = time.Now().UTC()
	if req.Purpose == "" {
		req.Purpose = "login"
	}
	if req.Channel == "" {
		req.Channel = "sms"
	}
	if req.Destination == "" {
		req.Destination = req.Phone
	}
	return r.pool.QueryRow(ctx, `
		INSERT INTO otp_requests (phone, otp_hash, attempts, expires_at, used, purpose, batch_id, channel, destination, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		req.Phone, req.OTPHash, req.Attempts, req.ExpiresAt, req.Used, req.Purpose, req.BatchID, req.Channel, req.Destination, req.CreatedAt,
	).Scan(&req.ID)
}

// CreateWithQuota atomically checks rate limits and inserts an OTP row under a transaction-scoped advisory lock.
// Using pg_advisory_xact_lock ensures full compatibility with transaction-pooling modes (e.g. PgBouncer),
// auto-releasing on commit or rollback.
func (r *OTPRepo) CreateWithQuota(ctx context.Context, req *OTPRequest, window time.Duration, maxPerWindow int, cooldownWindow time.Duration) error {
	req.CreatedAt = time.Now().UTC()
	if req.Purpose == "" {
		req.Purpose = "login"
	}
	if req.Channel == "" {
		req.Channel = "sms"
	}
	if req.Destination == "" {
		req.Destination = req.Phone
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin otp quota tx: %w", err)
	}
	defer tx.Rollback(ctx)

	// Explicit ::text casts for PgBouncer / simple-protocol parameter binding:
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text || ':' || $2::text, 0))`, req.Phone, req.Purpose)
	if err != nil {
		return fmt.Errorf("acquire otp advisory lock: %w", err)
	}

	now := time.Now().UTC()

	// 1. Enforce cooldown window
	if cooldownWindow > 0 {
		cooldownSince := now.Add(-cooldownWindow)
		var lastCreatedAt time.Time
		err := tx.QueryRow(ctx, `
			SELECT created_at FROM otp_requests
			WHERE phone = $1::text
			  AND ($2::text = '' OR purpose = $2::text)
			  AND created_at >= $3
			ORDER BY created_at DESC LIMIT 1`, req.Phone, req.Purpose, cooldownSince).Scan(&lastCreatedAt)
		if err == nil {
			remaining := cooldownWindow - now.Sub(lastCreatedAt)
			if remaining < time.Second {
				remaining = time.Second
			}
			return &RateLimitError{RetryAfter: remaining}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("check otp cooldown: %w", err)
		}
	}

	// 2. Enforce window quota
	since := now.Add(-window)
	var count int
	var oldestInWindow time.Time
	rows, err := tx.Query(ctx, `
		SELECT created_at FROM otp_requests
		WHERE phone = $1::text
		  AND ($2::text = '' OR purpose = $2::text)
		  AND created_at >= $3
		ORDER BY created_at ASC`, req.Phone, req.Purpose, since)
	if err != nil {
		return fmt.Errorf("query otp window count: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ca time.Time
		if err := rows.Scan(&ca); err != nil {
			return fmt.Errorf("scan otp created_at: %w", err)
		}
		if count == 0 {
			oldestInWindow = ca
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate otp created_at: %w", err)
	}

	if count >= maxPerWindow {
		remaining := window - now.Sub(oldestInWindow)
		if remaining < time.Second {
			remaining = time.Second
		}
		return &RateLimitError{RetryAfter: remaining}
	}

	// 3. Insert OTP row
	err = tx.QueryRow(ctx, `
		INSERT INTO otp_requests (phone, otp_hash, attempts, expires_at, used, purpose, batch_id, channel, destination, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		req.Phone, req.OTPHash, req.Attempts, req.ExpiresAt, req.Used, req.Purpose, req.BatchID, req.Channel, req.Destination, req.CreatedAt,
	).Scan(&req.ID)
	if err != nil {
		return fmt.Errorf("insert otp request: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit otp quota tx: %w", err)
	}
	return nil
}

func (r *OTPRepo) LatestUnused(ctx context.Context, phone string) (*OTPRequest, error) {
	return r.LatestUnusedByPurpose(ctx, phone, "", nil)
}

func (r *OTPRepo) LatestUnusedByPurpose(ctx context.Context, phone string, purpose string, batchID *uuid.UUID) (*OTPRequest, error) {
	var req OTPRequest
	var batchIDVal *uuid.UUID
	var purposeVal string
	var channelVal string
	var destinationVal sql.NullString
	err := r.pool.QueryRow(ctx, `
		SELECT id, phone, otp_hash, attempts, expires_at, used, COALESCE(purpose, 'login'), batch_id, COALESCE(channel, 'sms'), destination, created_at
		FROM otp_requests
		WHERE phone=$1 AND used=FALSE
		  AND ($2 = '' OR purpose=$2)
		  AND ($3::uuid IS NULL OR batch_id=$3)
		ORDER BY created_at DESC LIMIT 1`, phone, purpose, batchID).Scan(
		&req.ID, &req.Phone, &req.OTPHash, &req.Attempts, &req.ExpiresAt, &req.Used, &purposeVal, &batchIDVal, &channelVal, &destinationVal, &req.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	req.Purpose = purposeVal
	req.BatchID = batchIDVal
	req.Channel = channelVal
	if destinationVal.Valid {
		req.Destination = destinationVal.String
	} else {
		req.Destination = req.Phone
	}
	return &req, nil
}

func (r *OTPRepo) IncrementAttempts(ctx context.Context, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `UPDATE otp_requests SET attempts = attempts + 1 WHERE id=$1 AND attempts < 5`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrOTPLocked
	}
	return nil
}

func (r *OTPRepo) MarkUsed(ctx context.Context, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `UPDATE otp_requests SET used=TRUE WHERE id=$1 AND used=FALSE`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrOTPAlreadyUsed
	}
	return nil
}

func (r *OTPRepo) CountRecent(ctx context.Context, phone string, since time.Time) (int, error) {
	return r.CountRecentByPurpose(ctx, phone, "", since)
}

func (r *OTPRepo) CountRecentByPurpose(ctx context.Context, phone string, purpose string, since time.Time) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM otp_requests
		WHERE phone=$1
		  AND ($2 = '' OR purpose=$2)
		  AND created_at >= $3`, phone, purpose, since).Scan(&n)
	return n, err
}

type TokenRepo struct{ pool *pgxpool.Pool }

func NewTokenRepo(pool *pgxpool.Pool) *TokenRepo { return &TokenRepo{pool: pool} }

func (r *TokenRepo) Create(ctx context.Context, t *domain.PaymentToken) error {
	t.CreatedAt = time.Now().UTC()
	return r.pool.QueryRow(ctx, `
		INSERT INTO payment_tokens (due_id, token_hash, expires_at, used, created_at)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		t.DueID, t.TokenHash, t.ExpiresAt, t.Used, t.CreatedAt,
	).Scan(&t.ID)
}

func (r *TokenRepo) InvalidateUnusedForDue(ctx context.Context, dueID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `
		UPDATE payment_tokens SET used=TRUE WHERE due_id=$1 AND used=FALSE`, dueID)
	return err
}

func (r *TokenRepo) GetByHash(ctx context.Context, hash string) (*domain.PaymentToken, error) {
	var t domain.PaymentToken
	err := r.pool.QueryRow(ctx, `
		SELECT id, due_id, token_hash, expires_at, used, created_at
		FROM payment_tokens WHERE token_hash=$1`, hash).Scan(
		&t.ID, &t.DueID, &t.TokenHash, &t.ExpiresAt, &t.Used, &t.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (r *TokenRepo) MarkUsed(ctx context.Context, id uuid.UUID) error {
	tag, err := r.pool.Exec(ctx, `UPDATE payment_tokens SET used=TRUE WHERE id=$1 AND used=FALSE`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrTokenAlreadyUsed
	}
	return nil
}

type PushRepo struct{ pool *pgxpool.Pool }

func NewPushRepo(pool *pgxpool.Pool) *PushRepo { return &PushRepo{pool: pool} }

type PushSubscription struct {
	ID        uuid.UUID
	TenantID  uuid.UUID
	Endpoint  string
	P256dh    string
	Auth      string
	CreatedAt time.Time
}

func (r *PushRepo) Upsert(ctx context.Context, s *PushSubscription) error {
	s.CreatedAt = time.Now().UTC()
	err := r.pool.QueryRow(ctx, `
		INSERT INTO push_subscriptions (tenant_id, endpoint, p256dh, auth, created_at)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (endpoint) DO UPDATE 
		SET p256dh=EXCLUDED.p256dh, auth=EXCLUDED.auth
		WHERE push_subscriptions.tenant_id = EXCLUDED.tenant_id
		RETURNING id`,
		s.TenantID, s.Endpoint, s.P256dh, s.Auth, s.CreatedAt,
	).Scan(&s.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPushEndpointOwned
	}
	return err
}

func (r *PushRepo) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]PushSubscription, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id, tenant_id, endpoint, p256dh, auth, created_at
		FROM push_subscriptions WHERE tenant_id=$1`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PushSubscription
	for rows.Next() {
		var s PushSubscription
		if err := rows.Scan(&s.ID, &s.TenantID, &s.Endpoint, &s.P256dh, &s.Auth, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *PushRepo) DeleteByEndpoint(ctx context.Context, endpoint string) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE endpoint=$1`, endpoint)
	return err
}

func (r *PushRepo) DeleteByTenant(ctx context.Context, tenantID uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `DELETE FROM push_subscriptions WHERE tenant_id=$1`, tenantID)
	return err
}

type ReminderRepo struct{ pool *pgxpool.Pool }

func NewReminderRepo(pool *pgxpool.Pool) *ReminderRepo { return &ReminderRepo{pool: pool} }

func (r *ReminderRepo) TryLog(ctx context.Context, dueID uuid.UUID, reminderType, channel string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `
		INSERT INTO reminder_logs (due_id, reminder_type, channel)
		VALUES ($1,$2,$3)
		ON CONFLICT (due_id, reminder_type, channel) DO NOTHING`, dueID, reminderType, channel)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (r *ReminderRepo) Exists(ctx context.Context, dueID uuid.UUID, reminderType, channel string) (bool, error) {
	var ok bool
	err := r.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM reminder_logs
			WHERE due_id=$1 AND reminder_type=$2 AND channel=$3
		)`, dueID, reminderType, channel).Scan(&ok)
	return ok, err
}

func (r *ReminderRepo) DeleteLog(ctx context.Context, dueID uuid.UUID, reminderType, channel string) error {
	_, err := r.pool.Exec(ctx, `
		DELETE FROM reminder_logs
		WHERE due_id=$1 AND reminder_type=$2 AND channel=$3`, dueID, reminderType, channel)
	return err
}

type ImportRepo struct{ db DBTX }

func NewImportRepo(db DBTX) *ImportRepo { return &ImportRepo{db: db} }

type ImportLog struct {
	ID         uuid.UUID
	PropertyID uuid.UUID
	Filename   string
	RowCount   int
	ImportedAt time.Time
	ImportedBy uuid.UUID
}

func (r *ImportRepo) Create(ctx context.Context, l *ImportLog) error {
	l.ImportedAt = time.Now().UTC()
	return r.db.QueryRow(ctx, `
		INSERT INTO import_logs (property_id, filename, row_count, imported_at, imported_by)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		l.PropertyID, l.Filename, l.RowCount, l.ImportedAt, l.ImportedBy,
	).Scan(&l.ID)
}

func (r *ImportRepo) LatestImportedAt(ctx context.Context, propertyID uuid.UUID) (*time.Time, error) {
	var t *time.Time
	err := r.db.QueryRow(ctx, `
		SELECT imported_at FROM import_logs
		WHERE property_id=$1 ORDER BY imported_at DESC LIMIT 1`, propertyID).Scan(&t)
	if err != nil {
		return nil, err
	}
	return t, nil
}
