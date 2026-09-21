package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var ErrInvalidLocale = errors.New("invalid or unsupported locale")

type PreferencesRepo struct {
	db DBTX
}

func NewPreferencesRepo(db DBTX) *PreferencesRepo {
	return &PreferencesRepo{db: db}
}

func (r *PreferencesRepo) WithTx(tx pgx.Tx) *PreferencesRepo {
	return &PreferencesRepo{db: tx}
}

// GetByUserID returns the user preferences or pgx.ErrNoRows if not found.
func (r *PreferencesRepo) GetByUserID(ctx context.Context, userID uuid.UUID) (*domain.UserPreferences, error) {
	var pref domain.UserPreferences
	err := r.db.QueryRow(ctx, `
		SELECT user_id, locale, created_at, updated_at
		FROM user_preferences
		WHERE user_id = $1`, userID,
	).Scan(&pref.UserID, &pref.Locale, &pref.CreatedAt, &pref.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &pref, nil
}

// Upsert inserts or updates the user's preferred locale.
func (r *PreferencesRepo) Upsert(ctx context.Context, userID uuid.UUID, locale string) (*domain.UserPreferences, error) {
	now := time.Now().UTC()
	var pref domain.UserPreferences
	err := r.db.QueryRow(ctx, `
		INSERT INTO user_preferences (user_id, locale, created_at, updated_at)
		VALUES ($1, $2, $3, $3)
		ON CONFLICT (user_id) DO UPDATE
		SET locale = EXCLUDED.locale, updated_at = EXCLUDED.updated_at
		RETURNING user_id, locale, created_at, updated_at`,
		userID, locale, now,
	).Scan(&pref.UserID, &pref.Locale, &pref.CreatedAt, &pref.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" {
			// Postgres check_violation
			return nil, ErrInvalidLocale
		}
		return nil, fmt.Errorf("upsert user preferences: %w", err)
	}
	return &pref, nil
}
