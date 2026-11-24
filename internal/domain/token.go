package domain

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrOTPAlreadyUsed       = errors.New("otp: already used")
	ErrTokenAlreadyUsed     = errors.New("token: already used")
	ErrRefreshTokenNotFound = errors.New("refresh token not found")
	ErrReplayDetected       = errors.New("refresh token replay detected")
	ErrRefreshTokenExpired  = errors.New("refresh token expired")
	// ErrGraceWindow is returned by RotateTokenTx when a concurrent or retransmitted request
	// replays a token that was consumed within the last RefreshGraceWindow (30 s) and the
	// token family is still valid. The returned *domain.RefreshToken carries the current active
	// leaf's UserID and FamilyID so the caller can issue a new access token.
	// The caller MUST NOT update the refresh cookie — the client's existing cookie is already
	// the valid successor token. No new refresh token row is inserted in this path.
	ErrGraceWindow = errors.New("refresh token grace window: access token refreshed, cookie unchanged")
)

const (
	PaymentTokenTTL    = 72 * time.Hour
	TokenByteLen       = 32
	RefreshGraceWindow = 30 * time.Second
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
	ID              uuid.UUID  `json:"id"`
	UserID          uuid.UUID  `json:"user_id"`
	FamilyID        uuid.UUID  `json:"family_id"`
	TokenHash       string     `json:"-"`
	ExpiresAt       time.Time  `json:"expires_at"`
	Revoked         bool       `json:"revoked"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
	FamilyStartedAt time.Time  `json:"family_started_at"`
	UserAgent       *string    `json:"user_agent,omitempty"`
	IPAddress       *string    `json:"ip_address,omitempty"`
	LastUsedAt      time.Time  `json:"last_used_at"`
	CreatedAt       time.Time  `json:"created_at"`
}

// ActiveSession represents an authenticated client session/device.
type ActiveSession struct {
	ID          uuid.UUID `json:"id"`
	DeviceLabel string    `json:"device_label"`
	UserAgent   string    `json:"user_agent"`
	IPAddress   string    `json:"ip_address"`
	LastSeen    time.Time `json:"last_seen"`
	CreatedAt   time.Time `json:"created_at"`
	IsCurrent   bool      `json:"is_current"`
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
