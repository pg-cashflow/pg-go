package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

var (
	ErrInvalidToken   = errors.New("auth: invalid or not found refresh token")
	ErrReplayDetected = domain.ErrReplayDetected
)

// RefreshTokenRepository manages persistence for refresh token family rotation.
type RefreshTokenRepository interface {
	StoreRefreshToken(ctx context.Context, rt *domain.RefreshToken) error
	GetRefreshTokenByHash(ctx context.Context, hash string) (*domain.RefreshToken, error)
	RevokeRefreshToken(ctx context.Context, id uuid.UUID) error
	RevokeFamily(ctx context.Context, familyID uuid.UUID) error
	RevokeUserTokens(ctx context.Context, userID uuid.UUID) error
	RotateTokenTx(ctx context.Context, oldHash string, newRT *domain.RefreshToken) (*domain.RefreshToken, error)
	PurgeExpiredTokens(ctx context.Context, olderThan time.Duration) (int64, error)
}

// GenerateRefreshToken creates a high-entropy 256-bit cryptographically secure token
// and returns both the plaintext (to send via cookie) and hex SHA-256 hash (to store in DB).
func GenerateRefreshToken() (plaintext string, hash string, err error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return "", "", err
	}
	plaintext = hex.EncodeToString(bytes)
	h := sha256.Sum256([]byte(plaintext))
	hash = hex.EncodeToString(h[:])
	return plaintext, hash, nil
}

// HashRefreshToken calculates the SHA-256 hex digest of a plaintext refresh token.
func HashRefreshToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
