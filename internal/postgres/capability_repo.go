package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrCapabilityDenied = errors.New("postgres: manager capability denied or revoked")
	ErrManagerInactive  = errors.New("postgres: manager inactive or property mismatch")
)

// CapabilityRepo reads manager capability grants.
type CapabilityRepo struct{ db DBTX }

func NewCapabilityRepo(db DBTX) *CapabilityRepo { return &CapabilityRepo{db: db} }

// AssertCapabilityTx verifies manager authorization inside an ongoing database transaction.
// It executes hierarchical two-phase locking (2PL):
// Level 1: Lock manager's users row with FOR SHARE.
// Level 2: Lock manager_capabilities row with FOR SHARE.
//
// The caller must provide actorRole from the authenticated server context, not user input.
// If actorRole == domain.RoleOwner, the check returns nil immediately.
// If actorRole != domain.RoleManager, it returns ErrManagerInactive.
func AssertCapabilityTx(ctx context.Context, tx DBTX, propertyID, userID uuid.UUID, actorRole, capability string) error {
	if actorRole == string(domain.RoleOwner) || actorRole == "owner" {
		return nil
	}
	if actorRole != string(domain.RoleManager) && actorRole != "manager" {
		return ErrManagerInactive
	}

	// Level 1: Lock manager's users row with FOR SHARE
	var role string
	var propID *uuid.UUID
	err := tx.QueryRow(ctx, `
		SELECT role, property_id
		FROM users
		WHERE id = $1
		FOR SHARE`, userID,
	).Scan(&role, &propID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrManagerInactive
		}
		return fmt.Errorf("assert capability lock user: %w", err)
	}
	if role != string(domain.RoleManager) || propID == nil || *propID != propertyID {
		return ErrManagerInactive
	}

	// Level 2: Lock manager_capabilities row with FOR SHARE
	var one int
	err = tx.QueryRow(ctx, `
		SELECT 1
		FROM manager_capabilities
		WHERE property_id = $1
		  AND user_id = $2
		  AND capability = $3
		  AND revoked_at IS NULL
		FOR SHARE`, propertyID, userID, capability,
	).Scan(&one)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrCapabilityDenied
		}
		return fmt.Errorf("assert capability lock capability: %w", err)
	}
	return nil
}

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
	if out == nil {
		out = []string{}
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

// GrantTx creates or reactivates a capability grant inside an ongoing transaction with Level 1 locking.
func (r *CapabilityRepo) GrantTx(ctx context.Context, tx DBTX, propertyID, userID uuid.UUID, capability string, grantedBy uuid.UUID) error {
	var uid uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&uid); err != nil {
		return fmt.Errorf("grant capability lock user: %w", err)
	}
	_, err := tx.Exec(ctx, `
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

// RevokeTx marks a capability grant as revoked inside an ongoing transaction with Level 1 locking.
func (r *CapabilityRepo) RevokeTx(ctx context.Context, tx DBTX, propertyID, userID uuid.UUID, capability string) error {
	var uid uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&uid); err != nil {
		return fmt.Errorf("revoke capability lock user: %w", err)
	}
	_, err := tx.Exec(ctx, `
		UPDATE manager_capabilities SET revoked_at = NOW()
		WHERE property_id = $1 AND user_id = $2 AND capability = $3 AND revoked_at IS NULL`,
		propertyID, userID, capability)
	return err
}

// RevokeAll revokes every active grant for the manager on the property.
func (r *CapabilityRepo) RevokeAll(ctx context.Context, propertyID, userID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		UPDATE manager_capabilities SET revoked_at = NOW()
		WHERE property_id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		propertyID, userID)
	return err
}

// RevokeAllTx marks all capability grants as revoked inside an ongoing transaction with Level 1 locking.
func (r *CapabilityRepo) RevokeAllTx(ctx context.Context, tx DBTX, propertyID, userID uuid.UUID) error {
	var uid uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 FOR UPDATE`, userID).Scan(&uid); err != nil {
		return fmt.Errorf("revoke all capabilities lock user: %w", err)
	}
	_, err := tx.Exec(ctx, `
		UPDATE manager_capabilities SET revoked_at = NOW()
		WHERE property_id = $1 AND user_id = $2 AND revoked_at IS NULL`,
		propertyID, userID)
	return err
}
