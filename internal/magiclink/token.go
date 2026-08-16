package magiclink

import (
	"crypto/hmac"

	"github.com/pg-cashflow/pg-go/internal/domain"
)

// GenerateToken returns a raw hex token for the URL; never store raw.
func GenerateToken() (string, error) {
	return domain.GenerateToken()
}

// HashToken HMAC-SHA256 of raw token; raw never stored.
func HashToken(secret, raw string) string {
	return domain.HashToken(secret, raw)
}

// VerifyToken reports whether raw hashes to expected under secret.
func VerifyToken(secret, raw, expectedHash string) bool {
	got := HashToken(secret, raw)
	return hmac.Equal([]byte(got), []byte(expectedHash))
}
