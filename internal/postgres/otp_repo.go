package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type OTPRepo struct{ pool *pgxpool.Pool }

func NewOTPRepo(pool *pgxpool.Pool) *OTPRepo { return &OTPRepo{pool: pool} }

type OTPRequest struct {
	ID        uuid.UUID
	Phone     string
	OTPHash   string
	Attempts  int16
	ExpiresAt time.Time
	Used      bool
	Purpose   string
	BatchID   *uuid.UUID
	CreatedAt time.Time
}

func (r *OTPRepo) Create(ctx context.Context, req *OTPRequest) error {
	req.CreatedAt = time.Now().UTC()
	if req.Purpose == "" {
		req.Purpose = "login"
	}
	return r.pool.QueryRow(ctx, `
		INSERT INTO otp_requests (phone, otp_hash, attempts, expires_at, used, purpose, batch_id, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		req.Phone, req.OTPHash, req.Attempts, req.ExpiresAt, req.Used, req.Purpose, req.BatchID, req.CreatedAt,
	).Scan(&req.ID)
}

func (r *OTPRepo) LatestUnused(ctx context.Context, phone string) (*OTPRequest, error) {
	return r.LatestUnusedByPurpose(ctx, phone, "", nil)
}

func (r *OTPRepo) LatestUnusedByPurpose(ctx context.Context, phone string, purpose string, batchID *uuid.UUID) (*OTPRequest, error) {
	var req OTPRequest
	var batchIDVal *uuid.UUID
	var purposeVal string
	err := r.pool.QueryRow(ctx, `
		SELECT id, phone, otp_hash, attempts, expires_at, used, COALESCE(purpose, 'login'), batch_id, created_at
		FROM otp_requests
		WHERE phone=$1 AND used=FALSE
		  AND ($2 = '' OR purpose=$2)
		  AND ($3::uuid IS NULL OR batch_id=$3)
		ORDER BY created_at DESC LIMIT 1`, phone, purpose, batchID).Scan(
		&req.ID, &req.Phone, &req.OTPHash, &req.Attempts, &req.ExpiresAt, &req.Used, &purposeVal, &batchIDVal, &req.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	req.Purpose = purposeVal
	req.BatchID = batchIDVal
	return &req, nil
}

func (r *OTPRepo) IncrementAttempts(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE otp_requests SET attempts = attempts + 1 WHERE id=$1`, id)
	return err
}

func (r *OTPRepo) MarkUsed(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE otp_requests SET used=TRUE WHERE id=$1`, id)
	return err
}

func (r *OTPRepo) CountRecent(ctx context.Context, phone string, since time.Time) (int, error) {
	var n int
	err := r.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM otp_requests WHERE phone=$1 AND created_at >= $2`, phone, since).Scan(&n)
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
	_, err := r.pool.Exec(ctx, `UPDATE payment_tokens SET used=TRUE WHERE id=$1`, id)
	return err
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
	return r.pool.QueryRow(ctx, `
		INSERT INTO push_subscriptions (tenant_id, endpoint, p256dh, auth, created_at)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (endpoint) DO UPDATE SET tenant_id=EXCLUDED.tenant_id, p256dh=EXCLUDED.p256dh, auth=EXCLUDED.auth
		RETURNING id`,
		s.TenantID, s.Endpoint, s.P256dh, s.Auth, s.CreatedAt,
	).Scan(&s.ID)
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
