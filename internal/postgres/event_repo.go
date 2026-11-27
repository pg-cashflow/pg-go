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

const userCols = `id, phone, email, role, tenant_id, property_id, firebase_uid, token_version, created_at, last_login_at, disabled_at, COALESCE(profile, 'custom')`

func scanUser(row pgx.Row) (*domain.User, error) {
	var u domain.User
	var phone, email, profile sql.NullString
	err := row.Scan(&u.ID, &phone, &email, &u.Role, &u.TenantID, &u.PropertyID, &u.FirebaseUID, &u.TokenVersion, &u.CreatedAt, &u.LastLoginAt, &u.DisabledAt, &profile)
	if err != nil {
		return nil, err
	}
	if phone.Valid {
		u.Phone = phone.String
	}
	if email.Valid {
		u.Email = email.String
	}
	if profile.Valid {
		u.Profile = profile.String
	} else {
		u.Profile = "custom"
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
	profile := strings.TrimSpace(u.Profile)
	if profile == "" {
		profile = "custom"
	}
	return r.db.QueryRow(ctx, `
		INSERT INTO users (phone, email, role, tenant_id, property_id, firebase_uid, created_at, last_login_at, disabled_at, profile)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) RETURNING id`,
		phone, email, u.Role, u.TenantID, u.PropertyID, u.FirebaseUID, u.CreatedAt, u.LastLoginAt, u.DisabledAt, profile,
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
	var status string
	var u domain.User
	var phone, email, profile sql.NullString
	err := r.db.QueryRow(ctx, `
		SELECT ui.status,
		       u.id, u.phone, u.email, u.role, u.tenant_id, u.property_id, u.firebase_uid,
		       u.token_version, u.created_at, u.last_login_at, u.disabled_at, COALESCE(u.profile, 'custom')
		FROM user_identities ui
		JOIN users u ON u.id = ui.user_id
		WHERE ui.firebase_uid = $1
		ORDER BY (ui.status = 'active') DESC, ui.created_at DESC
		LIMIT 1`, firebaseUID).Scan(
		&status,
		&u.ID, &phone, &email, &u.Role, &u.TenantID, &u.PropertyID, &u.FirebaseUID,
		&u.TokenVersion, &u.CreatedAt, &u.LastLoginAt, &u.DisabledAt, &profile,
	)
	if err == nil {
		if status != "active" {
			// Revoked identity must NOT fall back to legacy users.firebase_uid.
			return nil, pgx.ErrNoRows
		}
		if phone.Valid {
			u.Phone = phone.String
		}
		if email.Valid {
			u.Email = email.String
		}
		if profile.Valid {
			u.Profile = profile.String
		} else {
			u.Profile = "custom"
		}
		if u.TokenVersion < 1 {
			u.TokenVersion = 1
		}
		return &u, nil
	}
	if err != pgx.ErrNoRows {
		return nil, err
	}
	return scanUser(r.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE firebase_uid=$1`, firebaseUID))
}

func (r *UserRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	return scanUser(r.db.QueryRow(ctx, `SELECT `+userCols+` FROM users WHERE id=$1`, id))
}

func (r *UserRepo) LinkFirebaseUID(ctx context.Context, userID uuid.UUID, firebaseUID string) error {
	return r.LinkUserIdentity(ctx, userID, "", firebaseUID, "firebase", "signup")
}

// ListUserIdentities returns all active/revoked external identities linked to userID.
func (r *UserRepo) ListUserIdentities(ctx context.Context, userID uuid.UUID) ([]domain.UserIdentity, error) {
	rows, err := r.db.Query(ctx, `
		SELECT id, user_id, firebase_project_id, firebase_uid, provider, status, link_method, created_at, linked_at, last_authenticated_at
		FROM user_identities
		WHERE user_id = $1
		ORDER BY created_at ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.UserIdentity
	for rows.Next() {
		var ui domain.UserIdentity
		if err := rows.Scan(
			&ui.ID, &ui.UserID, &ui.FirebaseProjectID, &ui.FirebaseUID,
			&ui.Provider, &ui.Status, &ui.LinkMethod, &ui.CreatedAt, &ui.LinkedAt, &ui.LastAuthenticatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, ui)
	}
	return out, rows.Err()
}

// LinkUserIdentity records an external Firebase UID for userID without overwriting any existing
// different user's mapping or silently replacing users.firebase_uid.
//
// All mutations run inside a single serialisable transaction so concurrent linking
// attempts on the same (project, uid) pair cannot both succeed. The INSERT after
// ON CONFLICT DO NOTHING is validated: if zero rows were inserted it means a
// revoked row (or a row for another user) already occupies the slot, which is an
// error rather than a silent success.
func (r *UserRepo) LinkUserIdentity(ctx context.Context, userID uuid.UUID, projectID, firebaseUID, provider, linkMethod string) error {
	if provider == "" {
		provider = "firebase"
	}
	if linkMethod == "" {
		linkMethod = "firebase_link"
	}

	// Acquire a transaction so the read-check and write are atomic.
	type beginner interface {
		Begin(context.Context) (pgx.Tx, error)
	}
	var tx pgx.Tx
	var err error
	if b, ok := r.db.(beginner); ok {
		tx, err = b.Begin(ctx)
		if err != nil {
			return fmt.Errorf("link identity: begin tx: %w", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
	} else if t, ok := r.db.(pgx.Tx); ok {
		tx = t
	} else {
		return fmt.Errorf("link identity: requires pool or tx")
	}

	// Lock any existing row for this (project, uid) pair regardless of status.
	var existingOwner uuid.UUID
	var existingStatus string
	err = tx.QueryRow(ctx, `
		SELECT user_id, status FROM user_identities
		WHERE firebase_project_id = $1 AND firebase_uid = $2
		FOR UPDATE`,
		projectID, firebaseUID,
	).Scan(&existingOwner, &existingStatus)

	if err == nil {
		// Row exists. If it belongs to a different user it cannot be reused.
		if existingOwner != userID {
			return fmt.Errorf("identity already linked to another user")
		}
		if existingStatus == "revoked" {
			// A deliberately revoked row must not silently re-activate.
			return fmt.Errorf("identity has been revoked and cannot be re-linked automatically")
		}
		// Touch last_authenticated_at for our own active row and commit.
		if _, err = tx.Exec(ctx, `
			UPDATE user_identities SET last_authenticated_at = NOW()
			WHERE firebase_project_id = $1 AND firebase_uid = $2`,
			projectID, firebaseUID); err != nil {
			return fmt.Errorf("link identity: touch auth ts: %w", err)
		}
		if b, ok := r.db.(beginner); ok {
			_ = b
			return tx.Commit(ctx)
		}
		return nil
	}
	if err != pgx.ErrNoRows {
		return fmt.Errorf("link identity: check existing: %w", err)
	}

	// No row exists yet. Update the legacy firebase_uid column only if it is
	// currently empty (COALESCE guard prevents overwriting an existing value).
	tag, err := tx.Exec(ctx, `
		UPDATE users
		SET firebase_uid = COALESCE(NULLIF(firebase_uid, ''), $2)
		WHERE id = $1`,
		userID, firebaseUID)
	if err != nil {
		return fmt.Errorf("link identity: update legacy uid: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("link identity: user not found %s", userID)
	}

	// Insert the canonical identity row. ON CONFLICT DO NOTHING is kept as a
	// safety net for a hypothetical concurrent insert on the same connection,
	// but we validate that a row was actually written.
	tag, err = tx.Exec(ctx, `
		INSERT INTO user_identities (user_id, firebase_project_id, firebase_uid, provider, status, link_method, last_authenticated_at)
		VALUES ($1, $2, $3, $4, 'active', $5, NOW())
		ON CONFLICT (firebase_project_id, firebase_uid) DO NOTHING`,
		userID, projectID, firebaseUID, provider, linkMethod)
	if err != nil {
		return fmt.Errorf("link identity: insert: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// The unique slot was claimed between our FOR UPDATE check and the INSERT.
		// This should be impossible under a serialisable transaction but we fail
		// loudly rather than silently treating a potential takeover as success.
		return fmt.Errorf("link identity: insert conflicted — concurrent linking detected for (project=%s uid=%s)", projectID, firebaseUID)
	}

	// Append-only audit event. Failure must roll back the entire identity link transaction.
	if _, err := tx.Exec(ctx, `
		INSERT INTO user_identity_audit_events (user_id, actor_user_id, event_type, firebase_project_id, firebase_uid, metadata)
		VALUES ($1, $1, 'identity_linked', $2, $3, jsonb_build_object('provider', $4::text, 'link_method', $5::text))`,
		userID, projectID, firebaseUID, provider, linkMethod); err != nil {
		return fmt.Errorf("link identity: append audit event: %w", err)
	}

	if _, ok := r.db.(beginner); ok {
		return tx.Commit(ctx)
	}
	return nil
}

// MergeUserIdentity atomically reassigns a secondary Firebase UID from sourceUserID to targetUserID
// and records an immutable audit event.
func (r *UserRepo) MergeUserIdentity(ctx context.Context, targetUserID, sourceUserID uuid.UUID, projectID, firebaseUID, provider string) error {
	if provider == "" {
		provider = "firebase"
	}
	var err error
	type beginner interface {
		Begin(context.Context) (pgx.Tx, error)
	}
	var tx pgx.Tx
	if b, ok := r.db.(beginner); ok {
		tx, err = b.Begin(ctx)
		if err != nil {
			return fmt.Errorf("begin merge identity tx: %w", err)
		}
		defer tx.Rollback(ctx)
	} else if t, ok := r.db.(pgx.Tx); ok {
		tx = t
	} else {
		return fmt.Errorf("merge user identity requires pool or tx")
	}

	// Lock identity row if present
	var existingOwner uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT user_id FROM user_identities
		WHERE firebase_project_id = $1 AND firebase_uid = $2
		FOR UPDATE`, projectID, firebaseUID).Scan(&existingOwner)
	if err != nil && err != pgx.ErrNoRows {
		return fmt.Errorf("lock identity for merge: %w", err)
	}
	if err == nil {
		if _, err := tx.Exec(ctx, `
			UPDATE user_identities
			SET user_id = $1, status = 'active', link_method = 'verified_merge', linked_at = NOW(), last_authenticated_at = NOW()
			WHERE firebase_project_id = $2 AND firebase_uid = $3`,
			targetUserID, projectID, firebaseUID); err != nil {
			return fmt.Errorf("reassign user identity: %w", err)
		}
	} else {
		if _, err := tx.Exec(ctx, `
			INSERT INTO user_identities (user_id, firebase_project_id, firebase_uid, provider, status, link_method, last_authenticated_at)
			VALUES ($1, $2, $3, $4, 'active', 'verified_merge', NOW())`,
			targetUserID, projectID, firebaseUID, provider); err != nil {
			return fmt.Errorf("insert merged user identity: %w", err)
		}
	}

	// Clear legacy firebase_uid on sourceUserID if it pointed to this UID so it can't shadow.
	if sourceUserID != uuid.Nil && sourceUserID != targetUserID {
		if _, err := tx.Exec(ctx, `UPDATE users SET firebase_uid = NULL WHERE id = $1 AND firebase_uid = $2`, sourceUserID, firebaseUID); err != nil {
			return fmt.Errorf("clear source legacy firebase_uid: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked = TRUE, revoked_at = NOW() WHERE user_id = $1 AND revoked = FALSE`, sourceUserID); err != nil {
			return fmt.Errorf("revoke source user tokens: %w", err)
		}
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO user_identity_audit_events (user_id, actor_user_id, event_type, firebase_project_id, firebase_uid, source_user_id, metadata)
		VALUES ($1, $1, 'identity_merged', $2, $3, NULLIF($4::text, '')::uuid, jsonb_build_object('provider', $5::text))`,
		targetUserID, projectID, firebaseUID, func() string {
			if sourceUserID == uuid.Nil {
				return ""
			}
			return sourceUserID.String()
		}(), provider); err != nil {
		return fmt.Errorf("audit identity merge: %w", err)
	}

	if _, ok := r.db.(beginner); ok {
		return tx.Commit(ctx)
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

// IsFamilyActive checks whether a refresh token family has at least one active, unrevoked, unexpired token.
func (r *UserRepo) IsFamilyActive(ctx context.Context, familyID uuid.UUID) (bool, error) {
	var active bool
	err := r.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM refresh_tokens
			WHERE family_id = $1 AND revoked = FALSE AND expires_at > NOW()
		)`, familyID).Scan(&active)
	return active, err
}

func (r *UserRepo) SetTenantID(ctx context.Context, userID, tenantID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `UPDATE users SET tenant_id=$2 WHERE id=$1`, userID, tenantID)
	return err
}

func (r *UserRepo) SetTenantIDAndPhone(ctx context.Context, userID, tenantID uuid.UUID, phone string) error {
	if phone != "" && !strings.HasPrefix(phone, "+G") {
		_, err := r.db.Exec(ctx, `UPDATE users SET tenant_id=$2, phone=COALESCE(NULLIF(phone,''), $3) WHERE id=$1`, userID, tenantID, phone)
		return err
	}
	_, err := r.db.Exec(ctx, `UPDATE users SET tenant_id=$2 WHERE id=$1`, userID, tenantID)
	return err
}

func (r *UserRepo) SetPropertyID(ctx context.Context, userID, propertyID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `UPDATE users SET property_id=$2 WHERE id=$1`, userID, propertyID)
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
