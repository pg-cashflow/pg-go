package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// CalendarTokenRepo manages property calendar authentication tokens.
// Operates on property_calendar_tokens with Row Level Security.
type CalendarTokenRepo struct {
	db DBTX
}

func NewCalendarTokenRepo(db DBTX) *CalendarTokenRepo {
	return &CalendarTokenRepo{db: db}
}

// InsertToken records a hashed calendar feed token.
func (r *CalendarTokenRepo) InsertToken(ctx context.Context, propertyID uuid.UUID, tokenHash string, expiresAt time.Time) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO property_calendar_tokens (property_id, token_hash, expires_at)
		VALUES ($1, $2, $3)
		ON CONFLICT (token_hash) DO NOTHING
	`, propertyID, tokenHash, expiresAt)
	return err
}

// GetTokenRevocation retrieves the revocation status and existence of a token for a property.
func (r *CalendarTokenRepo) GetTokenRevocation(ctx context.Context, propertyID uuid.UUID, tokenHash string) (revoked bool, exists bool, err error) {
	var revokedAt *time.Time
	err = r.db.QueryRow(ctx, `
		SELECT revoked_at FROM property_calendar_tokens
		WHERE property_id = $1 AND token_hash = $2
	`, propertyID, tokenHash).Scan(&revokedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, false, nil
		}
		return false, false, err
	}
	return revokedAt != nil, true, nil
}

// RevokeTokens marks all active tokens for a property as revoked.
func (r *CalendarTokenRepo) RevokeTokens(ctx context.Context, propertyID uuid.UUID) (int64, error) {
	tag, err := r.db.Exec(ctx, `
		UPDATE property_calendar_tokens
		SET revoked_at = NOW()
		WHERE property_id = $1 AND revoked_at IS NULL
	`, propertyID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
