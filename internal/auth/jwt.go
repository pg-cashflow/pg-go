package auth

import (
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

const TokenTTL = 30 * 24 * time.Hour

// DefaultTokenVersion matches users.token_version DEFAULT 1.
const DefaultTokenVersion = 1

// Claims are embedded in JWTs issued after successful OTP verification.
type Claims struct {
	UserID       uuid.UUID   `json:"user_id"`
	Role         domain.Role `json:"role"`
	TenantID     *uuid.UUID  `json:"tenant_id,omitempty"`
	PropertyID   *uuid.UUID  `json:"property_id,omitempty"`
	TokenVersion int         `json:"token_version"`
	jwt.RegisteredClaims
}

func tokenVersionOf(user *domain.User) int {
	if user == nil || user.TokenVersion < DefaultTokenVersion {
		return DefaultTokenVersion
	}
	return user.TokenVersion
}

// IssueToken creates a signed JWT with a 30-day TTL.
func IssueToken(secret string, user *domain.User) (string, error) {
	now := time.Now().UTC()
	claims := Claims{
		UserID:       user.ID,
		Role:         user.Role,
		TenantID:     user.TenantID,
		PropertyID:   user.PropertyID,
		TokenVersion: tokenVersionOf(user),
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   user.ID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(TokenTTL)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signed, nil
}

// VerifyToken parses and validates a JWT, returning its claims.
func VerifyToken(secret, tokenString string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenString, &Claims{}, func(t *jwt.Token) (any, error) {
		if t.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return []byte(secret), nil
	})
	if err != nil {
		return nil, fmt.Errorf("parse token: %w", err)
	}
	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}
	return claims, nil
}
