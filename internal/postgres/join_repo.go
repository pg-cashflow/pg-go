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
	status, tenant_id, slot_invite_id,
	occupation_type, date_of_birth, profile_schema_version, occupation_details, emergency_contact_name,
	created_at, updated_at`

func scanJoin(row pgx.Row) (*domain.JoinRequest, error) {
	var j domain.JoinRequest
	var occType *string
	err := row.Scan(
		&j.ID, &j.PropertyID, &j.UserID, &j.Phone, &j.Name, &j.AadhaarLast4,
		&j.PermanentAddress, &j.CurrentAddress, &j.ParentName, &j.EmergencyPhone, &j.JoinedOn,
		&j.Status, &j.TenantID, &j.SlotInviteID,
		&occType, &j.DateOfBirth, &j.ProfileSchemaVersion, &j.OccupationDetails, &j.EmergencyContactName,
		&j.CreatedAt, &j.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if occType != nil {
		j.OccupationType = domain.OccupationType(*occType)
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
	if j.ProfileSchemaVersion == 0 {
		j.ProfileSchemaVersion = 1
	}
	var occType *string
	if j.OccupationType != "" {
		s := string(j.OccupationType)
		occType = &s
	}
	occDetails := j.OccupationDetails
	if len(occDetails) == 0 {
		occDetails = json.RawMessage("{}")
	}
	return r.db.QueryRow(ctx, `
		INSERT INTO join_requests (
			property_id, user_id, phone, name, aadhaar_last4,
			permanent_address, current_address, parent_name, emergency_phone, joined_on,
			status, tenant_id, slot_invite_id,
			occupation_type, date_of_birth, profile_schema_version, occupation_details, emergency_contact_name,
			created_at, updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		ON CONFLICT (property_id, phone) WHERE status = 'pending'
		DO UPDATE SET user_id = EXCLUDED.user_id, slot_invite_id = COALESCE(EXCLUDED.slot_invite_id, join_requests.slot_invite_id), updated_at = EXCLUDED.updated_at
		RETURNING id`,
		j.PropertyID, j.UserID, j.Phone, j.Name, j.AadhaarLast4,
		j.PermanentAddress, j.CurrentAddress, j.ParentName, j.EmergencyPhone, j.JoinedOn,
		j.Status, j.TenantID, j.SlotInviteID,
		occType, j.DateOfBirth, j.ProfileSchemaVersion, occDetails, j.EmergencyContactName,
		j.CreatedAt, j.UpdatedAt,
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
	var occType *string
	if j.OccupationType != "" {
		s := string(j.OccupationType)
		occType = &s
	}
	occDetails := j.OccupationDetails
	if len(occDetails) == 0 {
		occDetails = json.RawMessage("{}")
	}
	_, err := r.db.Exec(ctx, `
		UPDATE join_requests SET name=$2, aadhaar_last4=$3,
			permanent_address=$4, current_address=$5, parent_name=$6, emergency_phone=$7, joined_on=$8,
			status=$9, tenant_id=$10, slot_invite_id=$11,
			occupation_type=$12, date_of_birth=$13, profile_schema_version=$14, occupation_details=$15, emergency_contact_name=$16,
			updated_at=$17
		WHERE id=$1`,
		j.ID, j.Name, j.AadhaarLast4,
		j.PermanentAddress, j.CurrentAddress, j.ParentName, j.EmergencyPhone, j.JoinedOn,
		j.Status, j.TenantID, j.SlotInviteID,
		occType, j.DateOfBirth, j.ProfileSchemaVersion, occDetails, j.EmergencyContactName,
		j.UpdatedAt,
	)
	return err
}

// RejectStalePending marks abandoned pending join rows as rejected.
func (r *JoinRepo) RejectStalePending(ctx context.Context, olderThan time.Time) (int64, error) {
	tag, err := r.db.Exec(ctx, `
		UPDATE join_requests SET status='rejected', updated_at=NOW()
		WHERE status='pending' AND tenant_id IS NULL AND created_at < $1`, olderThan)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
