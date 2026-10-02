package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrRefreshTokenNotFound = errors.New("refresh token not found")
	ErrReplayDetected       = errors.New("refresh token replay detected")
	ErrRefreshTokenExpired  = errors.New("refresh token expired")
)

type RefreshTokenRepo struct {
	pool *pgxpool.Pool
	db   DBTX
}

func NewRefreshTokenRepo(db DBTX) *RefreshTokenRepo {
	repo := &RefreshTokenRepo{db: db}
	if pool, ok := db.(*pgxpool.Pool); ok {
		repo.pool = pool
	}
	return repo
}

func (r *RefreshTokenRepo) WithTx(tx pgx.Tx) *RefreshTokenRepo {
	return &RefreshTokenRepo{pool: r.pool, db: tx}
}

func (r *RefreshTokenRepo) StoreRefreshToken(ctx context.Context, rt *domain.RefreshToken) error {
	if rt.ID == uuid.Nil {
		rt.ID = uuid.New()
	}
	if rt.CreatedAt.IsZero() {
		rt.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO refresh_tokens (id, user_id, family_id, token_hash, expires_at, revoked, revoked_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		rt.ID, rt.UserID, rt.FamilyID, rt.TokenHash, rt.ExpiresAt, rt.Revoked, rt.RevokedAt, rt.CreatedAt,
	)
	return err
}

func (r *RefreshTokenRepo) GetRefreshTokenByHash(ctx context.Context, hash string) (*domain.RefreshToken, error) {
	var rt domain.RefreshToken
	err := r.db.QueryRow(ctx, `
		SELECT id, user_id, family_id, token_hash, expires_at, revoked, revoked_at, created_at
		FROM refresh_tokens
		WHERE token_hash = $1`, hash,
	).Scan(&rt.ID, &rt.UserID, &rt.FamilyID, &rt.TokenHash, &rt.ExpiresAt, &rt.Revoked, &rt.RevokedAt, &rt.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &rt, nil
}

func (r *RefreshTokenRepo) RevokeRefreshToken(ctx context.Context, id uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked = TRUE, revoked_at = NOW()
		WHERE id = $1`, id,
	)
	return err
}

func (r *RefreshTokenRepo) RevokeFamily(ctx context.Context, familyID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked = TRUE, revoked_at = NOW()
		WHERE family_id = $1`, familyID,
	)
	return err
}

func (r *RefreshTokenRepo) RevokeUserTokens(ctx context.Context, userID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked = TRUE, revoked_at = NOW()
		WHERE user_id = $1`, userID,
	)
	return err
}

// RotateTokenTx atomically validates, revokes, and rotates a refresh token inside a single transaction.
// Row-level locking (FOR UPDATE) eliminates race conditions, and a 30s grace window protects against network retransmits.
func (r *RefreshTokenRepo) RotateTokenTx(ctx context.Context, oldHash string, newRT *domain.RefreshToken) (*domain.RefreshToken, error) {
	var tx pgx.Tx
	var err error

	if r.pool != nil {
		tx, err = r.pool.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("begin rotate token tx: %w", err)
		}
		defer tx.Rollback(ctx)
	} else if txDb, ok := r.db.(pgx.Tx); ok {
		tx = txDb
	} else {
		return nil, errors.New("refresh token repo requires pool or transaction")
	}

	var current domain.RefreshToken
	query := `
		SELECT id, user_id, family_id, token_hash, expires_at, revoked, revoked_at, created_at
		FROM refresh_tokens
		WHERE token_hash = $1
		FOR UPDATE
	`
	err = tx.QueryRow(ctx, query, oldHash).Scan(
		&current.ID, &current.UserID, &current.FamilyID,
		&current.TokenHash, &current.ExpiresAt, &current.Revoked,
		&current.RevokedAt, &current.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRefreshTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select token for update: %w", err)
	}

	// 1. Replay attack check
	if current.Revoked {
		// Grace window check: if token was revoked within the last 30 seconds (lost response ACK or concurrent retry)
		if current.RevokedAt != nil && time.Since(*current.RevokedAt) <= 30*time.Second {
			var activeToken domain.RefreshToken
			activeQuery := `
				SELECT id, user_id, family_id, token_hash, expires_at, revoked, revoked_at, created_at
				FROM refresh_tokens
				WHERE family_id = $1 AND revoked = FALSE AND expires_at > NOW()
				ORDER BY created_at DESC
				LIMIT 1
			`
			if aerr := tx.QueryRow(ctx, activeQuery, current.FamilyID).Scan(
				&activeToken.ID, &activeToken.UserID, &activeToken.FamilyID,
				&activeToken.TokenHash, &activeToken.ExpiresAt, &activeToken.Revoked,
				&activeToken.RevokedAt, &activeToken.CreatedAt,
			); aerr == nil {
				if r.pool != nil {
					_ = tx.Commit(ctx)
				}
				return &activeToken, nil
			}
		}

		// Replay attack confirmed: nuke entire family!
		_, _ = tx.Exec(ctx, `UPDATE refresh_tokens SET revoked = TRUE, revoked_at = NOW() WHERE family_id = $1`, current.FamilyID)
		if r.pool != nil {
			_ = tx.Commit(ctx)
		}
		return nil, ErrReplayDetected
	}

	// 2. Expiry check
	if time.Now().UTC().After(current.ExpiresAt) {
		return nil, ErrRefreshTokenExpired
	}

	// 3. Mark current token revoked with timestamp
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked = TRUE, revoked_at = $1 WHERE id = $2`, now, current.ID); err != nil {
		return nil, fmt.Errorf("revoke current token: %w", err)
	}

	// 4. Insert new rotated token
	if newRT.ID == uuid.Nil {
		newRT.ID = uuid.New()
	}
	newRT.FamilyID = current.FamilyID
	newRT.UserID = current.UserID
	newRT.CreatedAt = now
	if _, err := tx.Exec(ctx, `
		INSERT INTO refresh_tokens (id, user_id, family_id, token_hash, expires_at, revoked, revoked_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		newRT.ID, newRT.UserID, newRT.FamilyID, newRT.TokenHash, newRT.ExpiresAt, false, nil, newRT.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("insert rotated token: %w", err)
	}

	if r.pool != nil {
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit rotate token tx: %w", err)
		}
	}

	return newRT, nil
}

// PurgeExpiredTokens purges expired or revoked tokens older than the retention duration.
func (r *RefreshTokenRepo) PurgeExpiredTokens(ctx context.Context, olderThan time.Duration) (int64, error) {
	cutoff := time.Now().UTC().Add(-olderThan)
	res, err := r.db.Exec(ctx, `
		DELETE FROM refresh_tokens
		WHERE (revoked = TRUE AND revoked_at < $1)
		   OR (expires_at < $1)
	`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected(), nil
}
