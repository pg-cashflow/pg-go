package domain

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/google/uuid"
)

const (
	PaymentTokenTTL = 72 * time.Hour
	TokenByteLen    = 32
)

type PaymentToken struct {
	ID        uuid.UUID `json:"id"`
	DueID     uuid.UUID `json:"due_id"`
	TokenHash string    `json:"-"`
	ExpiresAt time.Time `json:"expires_at"`
	Used      bool      `json:"used"`
	CreatedAt time.Time `json:"created_at"`
}

// RefreshToken represents a persisted refresh token with family tracking.
type RefreshToken struct {
	ID        uuid.UUID `json:"id"`
	UserID    uuid.UUID `json:"user_id"`
	FamilyID  uuid.UUID `json:"family_id"`
	TokenHash string    `json:"-"`
	ExpiresAt time.Time `json:"expires_at"`
	Revoked         bool       `json:"revoked"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
	FamilyStartedAt time.Time  `json:"family_started_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

// GenerateToken returns a raw token (hex) for the URL; never store raw.
func GenerateToken() (raw string, err error) {
	b := make([]byte, TokenByteLen)
	if _, err = rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// HashToken HMAC-SHA256 of raw token; raw never stored.
func HashToken(secret, raw string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(raw))
	return hex.EncodeToString(mac.Sum(nil))
}
