package auth

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestVerifyToken(t *testing.T) {
	const secret = "test-jwt-secret-key-with-32-chars!!"
	const wrongSecret = "other-jwt-secret-key-with-32-chars!"

	validUser := &domain.User{
		ID:           uuid.New(),
		Role:         domain.RoleOwner,
		TokenVersion: 1,
	}

	validToken, err := IssueAccessToken(secret, validUser)
	if err != nil {
		t.Fatalf("failed to issue valid token: %v", err)
	}

	// Helper to split JWT into header, payload, signature
	parts := strings.Split(validToken, ".")
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts in jwt, got %d", len(parts))
	}

	// 1. Expired token
	expiredClaims := Claims{
		UserID:       validUser.ID,
		Role:         validUser.Role,
		TokenVersion: 1,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   validUser.ID.String(),
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-2 * time.Hour)),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-1 * time.Hour)),
		},
	}
	expiredToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, expiredClaims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("failed to create expired token: %v", err)
	}

	// 2. Token with wrong secret
	wrongSecretToken, err := IssueAccessToken(wrongSecret, validUser)
	if err != nil {
		t.Fatalf("failed to create wrong secret token: %v", err)
	}

	// 3. Tampered payload
	tamperedPayloadBytes := []byte(`{"user_id":"` + validUser.ID.String() + `","role":"admin","token_version":1}`)
	tamperedPayload := base64.RawURLEncoding.EncodeToString(tamperedPayloadBytes)
	tamperedPayloadToken := parts[0] + "." + tamperedPayload + "." + parts[2]

	// 4. Tampered role claim in valid token body
	tamperedRoleToken := parts[0] + "." + parts[1] + "x." + parts[2]

	// 5. alg=none token
	noneToken := jwt.NewWithClaims(jwt.SigningMethodNone, validClaimsFor(validUser, time.Hour))
	noneTokenString, err := noneToken.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("failed to create none token: %v", err)
	}

	// 6. HS384 token
	hs384Token, err := jwt.NewWithClaims(jwt.SigningMethodHS384, validClaimsFor(validUser, time.Hour)).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("failed to create HS384 token: %v", err)
	}

	// 7. HS512 token
	hs512Token, err := jwt.NewWithClaims(jwt.SigningMethodHS512, validClaimsFor(validUser, time.Hour)).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("failed to create HS512 token: %v", err)
	}

	// 8. Missing exp token
	missingExpClaims := Claims{
		UserID:       validUser.ID,
		Role:         validUser.Role,
		TokenVersion: 1,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:  validUser.ID.String(),
			IssuedAt: jwt.NewNumericDate(time.Now()),
		},
	}
	missingExpToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, missingExpClaims).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("failed to create missing exp token: %v", err)
	}

	tests := []struct {
		name      string
		secret    string
		token     string
		wantErr   bool
		checkUser bool
	}{
		{
			name:      "valid token",
			secret:    secret,
			token:     validToken,
			wantErr:   false,
			checkUser: true,
		},
		{
			name:    "expired token",
			secret:  secret,
			token:   expiredToken,
			wantErr: true,
		},
		{
			name:    "wrong secret",
			secret:  secret,
			token:   wrongSecretToken,
			wantErr: true,
		},
		{
			name:    "tampered payload",
			secret:  secret,
			token:   tamperedPayloadToken,
			wantErr: true,
		},
		{
			name:    "tampered signature / role claim",
			secret:  secret,
			token:   tamperedRoleToken,
			wantErr: true,
		},
		{
			name:    "alg=none token",
			secret:  secret,
			token:   noneTokenString,
			wantErr: true,
		},
		{
			name:    "HS384 token",
			secret:  secret,
			token:   hs384Token,
			wantErr: true,
		},
		{
			name:    "HS512 token",
			secret:  secret,
			token:   hs512Token,
			wantErr: true,
		},
		{
			name:    "empty string token",
			secret:  secret,
			token:   "",
			wantErr: true,
		},
		{
			name:    "garbage string token",
			secret:  secret,
			token:   "not.a.valid.jwt.payload.string",
			wantErr: true,
		},
		{
			name:    "missing exp claim",
			secret:  secret,
			token:   missingExpToken,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims, err := VerifyToken(tt.secret, tt.token)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("VerifyToken() expected error for %s, got nil claims: %+v", tt.name, claims)
				}
				return
			}
			if err != nil {
				t.Fatalf("VerifyToken() unexpected error for %s: %v", tt.name, err)
			}
			if tt.checkUser {
				if claims.UserID != validUser.ID {
					t.Errorf("UserID = %v, want %v", claims.UserID, validUser.ID)
				}
				if claims.Role != validUser.Role {
					t.Errorf("Role = %v, want %v", claims.Role, validUser.Role)
				}
				if claims.TokenVersion != validUser.TokenVersion {
					t.Errorf("TokenVersion = %v, want %v", claims.TokenVersion, validUser.TokenVersion)
				}
			}
		})
	}
}

func validClaimsFor(u *domain.User, ttl time.Duration) Claims {
	now := time.Now().UTC()
	return Claims{
		UserID:       u.ID,
		Role:         u.Role,
		TokenVersion: 1,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   u.ID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
		},
	}
}
