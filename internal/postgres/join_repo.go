package postgres

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type JoinRepo struct{ db DBTX }

func NewJoinRepo(db DBTX) *JoinRepo { return &JoinRepo{db: db} }

func (r *JoinRepo) WithTx(tx pgx.Tx) *JoinRepo { return &JoinRepo{db: tx} }

const joinCols = `id, property_id, user_id, phone, name, aadhaar_last4,
	permanent_address, current_address, parent_name, emergency_phone, joined_on,
	status, tenant_id, created_at, updated_at`

func scanJoin(row pgx.Row) (*domain.JoinRequest, error) {
	var j domain.JoinRequest
	err := row.Scan(
		&j.ID, &j.PropertyID, &j.UserID, &j.Phone, &j.Name, &j.AadhaarLast4,
		&j.PermanentAddress, &j.CurrentAddress, &j.ParentName, &j.EmergencyPhone, &j.JoinedOn,
		&j.Status, &j.TenantID, &j.CreatedAt, &j.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	return &j, nil
}

func (r *JoinRepo) Create(ctx context.Context, j *domain.JoinRequest) error {
	now := time.Now().UTC()
	j.CreatedAt = now
	j.UpdatedAt = now
	if j.Status == "" {
		j.Status = domain.JoinPending
	}
	return r.db.QueryRow(ctx, `
		INSERT INTO join_requests (
			property_id, user_id, phone, name, aadhaar_last4,
			permanent_address, current_address, parent_name, emergency_phone, joined_on,
			status, tenant_id, created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING id`,
		j.PropertyID, j.UserID, j.Phone, j.Name, j.AadhaarLast4,
		j.PermanentAddress, j.CurrentAddress, j.ParentName, j.EmergencyPhone, j.JoinedOn,
		j.Status, j.TenantID, j.CreatedAt, j.UpdatedAt,
	).Scan(&j.ID)
}

func (r *JoinRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.JoinRequest, error) {
	return scanJoin(r.db.QueryRow(ctx, `SELECT `+joinCols+` FROM join_requests WHERE id=$1`, id))
}

func (r *JoinRepo) GetPendingByUser(ctx context.Context, userID uuid.UUID) (*domain.JoinRequest, error) {
	return scanJoin(r.db.QueryRow(ctx, `
		SELECT `+joinCols+` FROM join_requests WHERE user_id=$1 AND status='pending'
		ORDER BY created_at DESC LIMIT 1`, userID))
}

func (r *JoinRepo) GetLatestByUser(ctx context.Context, userID uuid.UUID) (*domain.JoinRequest, error) {
	return scanJoin(r.db.QueryRow(ctx, `
		SELECT `+joinCols+` FROM join_requests WHERE user_id=$1
		ORDER BY created_at DESC LIMIT 1`, userID))
}

func (r *JoinRepo) ListByProperty(ctx context.Context, propertyID uuid.UUID, status *domain.JoinStatus) ([]domain.JoinRequest, error) {
	q := `SELECT ` + joinCols + ` FROM join_requests WHERE property_id=$1`
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
	var out []domain.JoinRequest
	for rows.Next() {
		j, err := scanJoin(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

func (r *JoinRepo) Update(ctx context.Context, j *domain.JoinRequest) error {
	j.UpdatedAt = time.Now().UTC()
	_, err := r.db.Exec(ctx, `
		UPDATE join_requests SET name=$2, aadhaar_last4=$3,
			permanent_address=$4, current_address=$5, parent_name=$6, emergency_phone=$7, joined_on=$8,
			status=$9, tenant_id=$10, updated_at=$11
		WHERE id=$1`,
		j.ID, j.Name, j.AadhaarLast4,
		j.PermanentAddress, j.CurrentAddress, j.ParentName, j.EmergencyPhone, j.JoinedOn,
		j.Status, j.TenantID, j.UpdatedAt,
	)
	return err
}
