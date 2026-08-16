package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
)

// DLT not registered — risk accepted.

// GenerateOTP returns a cryptographically random 6-digit OTP (zero-padded).
func GenerateOTP() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", fmt.Errorf("generate otp: %w", err)
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// HashOTP returns HMAC-SHA256(secret, otp) as hex. Plaintext OTP is never stored.
func HashOTP(secret, otp string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(otp))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyOTP compares a plaintext OTP against a stored HMAC hash using constant-time comparison.
func VerifyOTP(secret, otp, hash string) bool {
	expected := HashOTP(secret, otp)
	return hmac.Equal([]byte(expected), []byte(hash))
}
