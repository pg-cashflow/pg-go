package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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

type UserRepo struct{ db DBTX }

func NewUserRepo(db DBTX) *UserRepo { return &UserRepo{db: db} }

func (r *UserRepo) WithTx(tx pgx.Tx) *UserRepo { return &UserRepo{db: tx} }

const userCols = `id, phone, email, role, tenant_id, property_id, firebase_uid, token_version, created_at, last_login_at`

func scanUser(row pgx.Row) (*domain.User, error) {
	var u domain.User
	var phone, email sql.NullString
	err := row.Scan(&u.ID, &phone, &email, &u.Role, &u.TenantID, &u.PropertyID, &u.FirebaseUID, &u.TokenVersion, &u.CreatedAt, &u.LastLoginAt)
	if err != nil {
		return nil, err
	}
	if phone.Valid {
		u.Phone = phone.String
	}
	if email.Valid {
		u.Email = email.String
	}
	if u.TokenVersion < 1 {
		u.TokenVersion = 1
	}
	return &u, nil
}

func nullIfEmptyStr(s string) *string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func (r *UserRepo) Create(ctx context.Context, u *domain.User) error {
	u.CreatedAt = time.Now().UTC()
	var email *string
	if strings.TrimSpace(u.Email) != "" {
		lower := strings.ToLower(strings.TrimSpace(u.Email))
		email = &lower
	}
	phone := nullIfEmptyStr(u.Phone)
	return r.db.QueryRow(ctx, `
		INSERT INTO users (phone, email, role, tenant_id, property_id, firebase_uid, created_at, last_login_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`,
		phone, email, u.Role, u.TenantID, u.PropertyID, u.FirebaseUID, u.CreatedAt, u.LastLoginAt,
	).Scan(&u.ID)
}

func (r *UserRepo) GetByPhone(ctx context.Context, phone string) (*domain.User, error) {
	return scanUser(r.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE phone=$1`, strings.TrimSpace(phone)))
}

func (r *UserRepo) GetByEmail(ctx context.Context, email string) (*domain.User, error) {
	clean := strings.ToLower(strings.TrimSpace(email))
	return scanUser(r.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE LOWER(email)=$1`, clean))
}

func (r *UserRepo) GetByFirebaseUID(ctx context.Context, firebaseUID string) (*domain.User, error) {
	return scanUser(r.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE firebase_uid=$1`, firebaseUID))
}

func (r *UserRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	return scanUser(r.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
}

func (r *UserRepo) LinkFirebaseUID(ctx context.Context, userID uuid.UUID, firebaseUID string) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE users SET firebase_uid=$2
		WHERE id=$1`,
		userID, firebaseUID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("user not found %s", userID)
	}
	return nil
}

func (r *UserRepo) TouchLogin(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.Exec(ctx, `UPDATE users SET last_login_at=NOW() WHERE id=$1`, id)
	return err
}

func (r *UserRepo) IncrementTokenVersion(ctx context.Context, id uuid.UUID) error {
	tag, err := r.db.Exec(ctx, `UPDATE users SET token_version = token_version + 1 WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("user not found %s", id)
	}
	return nil
}

func (r *UserRepo) SetTenantID(ctx context.Context, userID, tenantID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `UPDATE users SET tenant_id=$2 WHERE id=$1`, userID, tenantID)
	return err
}

// GetByPropertyAndRole returns all users with a given role scoped to a property.
// Used by the notification resolver to find owner(s) or manager(s) for a property.
// An empty result (len == 0) is not an error — a property may have no manager yet.
func (r *UserRepo) GetByPropertyAndRole(ctx context.Context, propertyID uuid.UUID, role domain.Role) ([]domain.User, error) {
	rows, err := r.db.Query(ctx,
		`SELECT `+userCols+` FROM users WHERE property_id=$1 AND role=$2`,
		propertyID, role)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}

// GetByTenantID returns the user account linked to a tenant record.
// Returns pgx.ErrNoRows if the tenant has no linked user account yet.
func (r *UserRepo) GetByTenantID(ctx context.Context, tenantID uuid.UUID) (*domain.User, error) {
	return scanUser(r.db.QueryRow(ctx,
		`SELECT `+userCols+` FROM users WHERE tenant_id=$1`, tenantID))
}
