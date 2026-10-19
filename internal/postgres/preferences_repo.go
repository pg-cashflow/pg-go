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
var ErrInvalidTheme = errors.New("invalid theme")

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
		SELECT user_id, locale, theme, created_at, updated_at
		FROM user_preferences
		WHERE user_id = $1`, userID,
	).Scan(&pref.UserID, &pref.Locale, &pref.Theme, &pref.CreatedAt, &pref.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &pref, nil
}

// Upsert inserts or partially updates the user's locale and/or theme.
// locale or theme may be nil to leave that field unchanged.
func (r *PreferencesRepo) Upsert(ctx context.Context, userID uuid.UUID, locale *string, theme *string) (*domain.UserPreferences, error) {
	now := time.Now().UTC()
	var pref domain.UserPreferences
	err := r.db.QueryRow(ctx, `
		INSERT INTO user_preferences (user_id, locale, theme, created_at, updated_at)
		VALUES ($1, COALESCE($2, 'en-IN'), $3, $4, $4)
		ON CONFLICT (user_id) DO UPDATE
		SET locale     = COALESCE($2, user_preferences.locale),
		    theme      = COALESCE($3, user_preferences.theme),
		    updated_at = EXCLUDED.updated_at
		RETURNING user_id, locale, theme, created_at, updated_at`,
		userID, locale, theme, now,
	).Scan(&pref.UserID, &pref.Locale, &pref.Theme, &pref.CreatedAt, &pref.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23514" {
			if pgErr.ConstraintName == "chk_user_preferences_theme" {
				return nil, ErrInvalidTheme
			}
			return nil, ErrInvalidLocale
		}
		return nil, fmt.Errorf("upsert user preferences: %w", err)
	}
	return &pref, nil
}
