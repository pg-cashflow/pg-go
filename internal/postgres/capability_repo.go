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
