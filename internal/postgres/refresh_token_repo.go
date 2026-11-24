package postgres

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrRefreshTokenNotFound = domain.ErrRefreshTokenNotFound
	ErrReplayDetected       = domain.ErrReplayDetected
	ErrRefreshTokenExpired  = domain.ErrRefreshTokenExpired
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
	if rt.FamilyStartedAt.IsZero() {
		rt.FamilyStartedAt = rt.CreatedAt
	}
	if rt.LastUsedAt.IsZero() {
		rt.LastUsedAt = rt.CreatedAt
	}
	_, err := r.db.Exec(ctx, `
		INSERT INTO refresh_tokens (id, user_id, family_id, token_hash, expires_at, revoked, revoked_at, family_started_at, user_agent, ip_address, last_used_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		rt.ID, rt.UserID, rt.FamilyID, rt.TokenHash, rt.ExpiresAt, rt.Revoked, rt.RevokedAt, rt.FamilyStartedAt, rt.UserAgent, rt.IPAddress, rt.LastUsedAt, rt.CreatedAt,
	)
	return err
}

func (r *RefreshTokenRepo) GetRefreshTokenByHash(ctx context.Context, hash string) (*domain.RefreshToken, error) {
	var rt domain.RefreshToken
	err := r.db.QueryRow(ctx, `
		SELECT id, user_id, family_id, token_hash, expires_at, revoked, revoked_at, family_started_at, user_agent, ip_address, last_used_at, created_at
		FROM refresh_tokens
		WHERE token_hash = $1`, hash,
	).Scan(&rt.ID, &rt.UserID, &rt.FamilyID, &rt.TokenHash, &rt.ExpiresAt, &rt.Revoked, &rt.RevokedAt, &rt.FamilyStartedAt, &rt.UserAgent, &rt.IPAddress, &rt.LastUsedAt, &rt.CreatedAt)
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

func (r *RefreshTokenRepo) RevokeFamilyByUser(ctx context.Context, userID, familyID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked = TRUE, revoked_at = NOW()
		WHERE user_id = $1 AND family_id = $2 AND revoked = FALSE`, userID, familyID,
	)
	return err
}

func (r *RefreshTokenRepo) RevokeAllExceptFamily(ctx context.Context, userID, exceptFamilyID uuid.UUID) error {
	_, err := r.db.Exec(ctx, `
		UPDATE refresh_tokens
		SET revoked = TRUE, revoked_at = NOW()
		WHERE user_id = $1 AND family_id != $2 AND revoked = FALSE`, userID, exceptFamilyID,
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

func (r *RefreshTokenRepo) ListActiveSessions(ctx context.Context, userID uuid.UUID) ([]domain.ActiveSession, error) {
	rows, err := r.db.Query(ctx, `
		SELECT DISTINCT ON (family_id)
			family_id,
			COALESCE(user_agent, ''),
			COALESCE(ip_address, ''),
			last_used_at,
			family_started_at
		FROM refresh_tokens
		WHERE user_id = $1
		  AND revoked = FALSE
		  AND expires_at > NOW()
		ORDER BY family_id, last_used_at DESC
	`, userID)
	if err != nil {
		return nil, fmt.Errorf("list active sessions: %w", err)
	}
	defer rows.Close()

	var sessions []domain.ActiveSession
	for rows.Next() {
		var s domain.ActiveSession
		if err := rows.Scan(&s.ID, &s.UserAgent, &s.IPAddress, &s.LastSeen, &s.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan active session: %w", err)
		}
		s.DeviceLabel = domain.ParseDeviceLabel(s.UserAgent)
		sessions = append(sessions, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].LastSeen.After(sessions[j].LastSeen)
	})
	return sessions, nil
}

// RotateTokenTx atomically validates, revokes, and rotates a refresh token inside a single transaction.
// Row-level locking (FOR UPDATE) eliminates race conditions, and a short grace window (domain.RefreshGraceWindow, 30s) protects against network retransmits.
//
// Grace-window idempotency invariant: within RefreshGraceWindow of consuming token T, every
// duplicate use of T must receive the same active successor without minting a new token.
// Returns (leafToken, domain.ErrGraceWindow) so the caller can issue a fresh access token
// without updating the refresh cookie — the client's existing cookie is already the valid successor.
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
	var replacedBy *uuid.UUID
	query := `
		SELECT id, user_id, family_id, token_hash, expires_at, revoked, revoked_at, family_started_at, created_at, replaced_by
		FROM refresh_tokens
		WHERE token_hash = $1
		FOR UPDATE
	`
	err = tx.QueryRow(ctx, query, oldHash).Scan(
		&current.ID, &current.UserID, &current.FamilyID,
		&current.TokenHash, &current.ExpiresAt, &current.Revoked,
		&current.RevokedAt, &current.FamilyStartedAt, &current.CreatedAt, &replacedBy,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrRefreshTokenNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select token for update: %w", err)
	}

	familyStarted := current.FamilyStartedAt
	if familyStarted.IsZero() {
		familyStarted = current.CreatedAt
	}

	// 1. Replay attack check
	if current.Revoked {
		// Check if the family has any active (unrevoked) tokens left
		var activeCount int
		if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM refresh_tokens WHERE family_id = $1 AND revoked = FALSE AND expires_at > NOW()`, current.FamilyID).Scan(&activeCount); err != nil {
			return nil, fmt.Errorf("check active family tokens: %w", err)
		}

		// Grace-window idempotency: only applies when the family still has active tokens,
		// the consumed token was revoked within RefreshGraceWindow, and a successor pointer exists.
		//
		// Design invariant: a duplicate use of a consumed token within the grace window must
		// return the SAME active successor without minting any new token. We walk the replaced_by
		// chain (capped at maxGraceChainDepth) to find the current unrevoked leaf, then return
		// (leaf, ErrGraceWindow). The caller issues a fresh access token from the leaf's UserID
		// but must NOT update the refresh cookie — the client's existing cookie is already the
		// valid successor. No INSERT or UPDATE is performed in this path.
		const maxGraceChainDepth = 16
		if activeCount > 0 && current.RevokedAt != nil && time.Since(*current.RevokedAt) <= domain.RefreshGraceWindow && replacedBy != nil {
			var leaf *domain.RefreshToken
			walkID := replacedBy
			for depth := 0; depth < maxGraceChainDepth && walkID != nil; depth++ {
				var node domain.RefreshToken
				var nodeReplacedBy *uuid.UUID
				err := tx.QueryRow(ctx, `
					SELECT id, user_id, family_id, token_hash, expires_at, revoked, revoked_at, family_started_at, created_at, replaced_by
					FROM refresh_tokens
					WHERE id = $1
					FOR UPDATE
				`, *walkID).Scan(
					&node.ID, &node.UserID, &node.FamilyID,
					&node.TokenHash, &node.ExpiresAt, &node.Revoked,
					&node.RevokedAt, &node.FamilyStartedAt, &node.CreatedAt, &nodeReplacedBy,
				)
				if err != nil {
					break // Node missing or query error — stop walk, fall through to family revoke
				}
				if !node.Revoked {
					leaf = &node
					break
				}
				walkID = nodeReplacedBy
			}

			if leaf != nil {
				// Found the current active leaf. Commit the read-only transaction and signal grace.
				if r.pool != nil {
					if err := tx.Commit(ctx); err != nil {
						return nil, fmt.Errorf("commit grace-window read tx: %w", err)
					}
				}
				return leaf, domain.ErrGraceWindow
			}
		}

		// Replay attack confirmed or family already nuked: ensure entire family is revoked!
		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked = TRUE, revoked_at = NOW() WHERE family_id = $1`, current.FamilyID); err != nil {
			return nil, fmt.Errorf("revoke compromised token family: %w", err)
		}
		if r.pool != nil {
			if err := tx.Commit(ctx); err != nil {
				return nil, fmt.Errorf("commit revoke family tx: %w", err)
			}
		}
		return nil, ErrReplayDetected
	}

	// 2. Absolute Maximum Session Lifetime check (90 days)
	// Uses immutable family_started_at so purging older revoked tokens never resets the ceiling
	const maxSessionFamilyLifetime = 90 * 24 * time.Hour
	if time.Since(familyStarted) > maxSessionFamilyLifetime {
		return nil, ErrRefreshTokenExpired
	}
	maxExpiry := familyStarted.Add(maxSessionFamilyLifetime)
	if newRT.ExpiresAt.After(maxExpiry) {
		newRT.ExpiresAt = maxExpiry
	}

	// 3. Expiry check
	if time.Now().UTC().After(current.ExpiresAt) {
		return nil, ErrRefreshTokenExpired
	}

	// 4. Insert new rotated token with inherited immutable family_started_at first so FK is satisfied
	now := time.Now().UTC()
	if newRT.ID == uuid.Nil {
		newRT.ID = uuid.New()
	}
	newRT.FamilyID = current.FamilyID
	newRT.UserID = current.UserID
	newRT.FamilyStartedAt = familyStarted
	newRT.CreatedAt = now
	newRT.LastUsedAt = now
	if newRT.UserAgent == nil {
		newRT.UserAgent = current.UserAgent
	}
	if newRT.IPAddress == nil {
		newRT.IPAddress = current.IPAddress
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO refresh_tokens (id, user_id, family_id, token_hash, expires_at, revoked, revoked_at, family_started_at, user_agent, ip_address, last_used_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		newRT.ID, newRT.UserID, newRT.FamilyID, newRT.TokenHash, newRT.ExpiresAt, false, nil, newRT.FamilyStartedAt, newRT.UserAgent, newRT.IPAddress, newRT.LastUsedAt, newRT.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("insert rotated token: %w", err)
	}

	// 5. Mark current token revoked with timestamp and pointer to successor token
	if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked = TRUE, revoked_at = $1, replaced_by = $2 WHERE id = $3`, now, newRT.ID, current.ID); err != nil {
		return nil, fmt.Errorf("revoke current token: %w", err)
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
