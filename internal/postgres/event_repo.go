package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type EventRepo struct{ db DBTX }

func NewEventRepo(db DBTX) *EventRepo { return &EventRepo{db: db} }

func (r *EventRepo) WithTx(tx pgx.Tx) *EventRepo { return &EventRepo{db: tx} }

func (r *EventRepo) Insert(ctx context.Context, e *domain.Event) error {
	if e.OccurredAt.IsZero() {
		e.OccurredAt = time.Now().UTC()
	}
	return r.db.QueryRow(ctx, `
		INSERT INTO events (tenant_id, property_id, event_type, due_id, occurred_at, payload)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id, created_at`,
		e.TenantID, e.PropertyID, e.EventType, e.DueID, e.OccurredAt, e.Payload,
	).Scan(&e.ID, &e.CreatedAt)
}

type EventFilter struct {
	PropertyID uuid.UUID
	TenantID   *uuid.UUID
	EventType  *domain.EventType
	From       *time.Time
	To         *time.Time
	Limit      int
}

func (r *EventRepo) List(ctx context.Context, f EventFilter) ([]domain.Event, error) {
	q := `SELECT id, tenant_id, property_id, event_type, due_id, occurred_at, payload, created_at
		FROM events WHERE property_id=$1`
	args := []any{f.PropertyID}
	n := 2
	if f.TenantID != nil {
		q += ` AND tenant_id=$` + itoa(n)
		args = append(args, *f.TenantID)
		n++
	}
	if f.EventType != nil {
		q += ` AND event_type=$` + itoa(n)
		args = append(args, *f.EventType)
		n++
	}
	if f.From != nil {
		q += ` AND occurred_at >= $` + itoa(n)
		args = append(args, *f.From)
		n++
	}
	if f.To != nil {
		q += ` AND occurred_at <= $` + itoa(n)
		args = append(args, *f.To)
		n++
	}
	q += ` ORDER BY occurred_at DESC`
	limit := f.Limit
	if limit <= 0 {
		limit = 200
	}
	q += ` LIMIT $` + itoa(n)
	args = append(args, limit)

	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Event
	for rows.Next() {
		var e domain.Event
		var payload []byte
		if err := rows.Scan(&e.ID, &e.TenantID, &e.PropertyID, &e.EventType, &e.DueID, &e.OccurredAt, &payload, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}

func itoa(n int) string {
	const digits = "0123456789"
	if n < 10 {
		return string(digits[n])
	}
	return itoa(n/10) + string(digits[n%10])
}

type UserRepo struct{ pool *pgxpool.Pool }

func NewUserRepo(pool *pgxpool.Pool) *UserRepo { return &UserRepo{pool: pool} }

const userCols = `id, phone, role, tenant_id, property_id, firebase_uid, created_at, last_login_at`

func scanUser(row pgx.Row) (*domain.User, error) {
	var u domain.User
	err := row.Scan(&u.ID, &u.Phone, &u.Role, &u.TenantID, &u.PropertyID, &u.FirebaseUID, &u.CreatedAt, &u.LastLoginAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (r *UserRepo) Create(ctx context.Context, u *domain.User) error {
	u.CreatedAt = time.Now().UTC()
	return r.pool.QueryRow(ctx, `
		INSERT INTO users (phone, role, tenant_id, property_id, firebase_uid, created_at, last_login_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		u.Phone, u.Role, u.TenantID, u.PropertyID, u.FirebaseUID, u.CreatedAt, u.LastLoginAt,
	).Scan(&u.ID)
}

func (r *UserRepo) GetByPhone(ctx context.Context, phone string) (*domain.User, error) {
	return scanUser(r.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE phone=$1`, phone))
}

func (r *UserRepo) GetByFirebaseUID(ctx context.Context, firebaseUID string) (*domain.User, error) {
	return scanUser(r.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE firebase_uid=$1`, firebaseUID))
}

func (r *UserRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	return scanUser(r.pool.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
}

func (r *UserRepo) LinkFirebaseUID(ctx context.Context, userID uuid.UUID, firebaseUID string) error {
	tag, err := r.pool.Exec(ctx, `
		UPDATE users SET firebase_uid=$2
		WHERE id=$1 AND (firebase_uid IS NULL OR firebase_uid=$2)`,
		userID, firebaseUID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("firebase uid conflict for user %s", userID)
	}
	return nil
}

func (r *UserRepo) TouchLogin(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE users SET last_login_at=NOW() WHERE id=$1`, id)
	return err
}
