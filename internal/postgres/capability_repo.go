package postgres

import (
	"context"

	"github.com/google/uuid"
)

// CapabilityRepo reads manager capability grants.
type CapabilityRepo struct{ db DBTX }

func NewCapabilityRepo(db DBTX) *CapabilityRepo { return &CapabilityRepo{db: db} }

// HasActive reports whether the user holds an unrevoked grant for the capability.
func (r *CapabilityRepo) HasActive(ctx context.Context, propertyID, userID uuid.UUID, capability string) (bool, error) {
	var ok bool
	err := r.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM manager_capabilities
			WHERE property_id = $1 AND user_id = $2 AND capability = $3 AND revoked_at IS NULL
		)`, propertyID, userID, capability).Scan(&ok)
	return ok, err
}

// ListActive returns unrevoked capability names for the user on the property.
func (r *CapabilityRepo) ListActive(ctx context.Context, propertyID, userID uuid.UUID) ([]string, error) {
	rows, err := r.db.Query(ctx, `
		SELECT capability FROM manager_capabilities
		WHERE property_id = $1 AND user_id = $2 AND revoked_at IS NULL
		ORDER BY capability`, propertyID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cap string
		if err := rows.Scan(&cap); err != nil {
			return nil, err
		}
		out = append(out, cap)
	}
	return out, rows.Err()
}

// Grant creates or reactivates a capability grant.
func (r *CapabilityRepo) Grant(ctx context.Context, propertyID, userID uuid.UUID, capability string, grantedBy uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO manager_capabilities (property_id, user_id, capability, granted_by, granted_at, revoked_at)
		VALUES ($1, $2, $3, $4, NOW(), NULL)
		ON CONFLICT (property_id, user_id, capability) DO UPDATE
		SET granted_by = EXCLUDED.granted_by, granted_at = NOW(), revoked_at = NULL`,
		propertyID, userID, capability, grantedBy)
	return err
}

// Revoke marks a single capability grant as revoked.
func (r *CapabilityRepo) Revoke(ctx context.Context, propertyID, userID uuid.UUID, capability string) error {
	tag, err := r.db.Exec(ctx, `
		UPDATE manager_capabilities SET revoked_at = NOW()
		WHERE property_id = $1 AND user_id = $2 AND capability = $3 AND revoked_at IS NULL`,
		propertyID, userID, capability)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	return nil
}

// RevokeAll revokes every active grant for the manager on the property.
func (r *CapabilityRepo) RevokeAll(ctx context.Context, propertyID, userID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		UPDATE manager_capabilities SET revoked_at = NOW()
		WHERE property_id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		propertyID, userID)
	return err
}
