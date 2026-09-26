package auth

import (
	"context"
	"fmt"
	"strings"
	"time"

	firebase "firebase.google.com/go/v4"
	fbauth "firebase.google.com/go/v4/auth"
	"google.golang.org/api/option"
)

// FirebaseIdentity is the verified Firebase Auth identity extracted from an ID token.
type FirebaseIdentity struct {
	UID           string
	Phone         string
	Email         string
	EmailVerified bool
	AuthTime      time.Time
}

// FirebaseVerifier validates Firebase ID tokens and extracts UID + phone/email.
type FirebaseVerifier struct {
	client *fbauth.Client
}

// NewFirebaseVerifier initializes Firebase Admin SDK for token verification.
func NewFirebaseVerifier(ctx context.Context, projectID, credentialsPath string) (*FirebaseVerifier, error) {
	if projectID == "" {
		return nil, fmt.Errorf("firebase project id is required")
	}
	var opts []option.ClientOption
	if credentialsPath != "" {
		opts = append(opts, option.WithCredentialsFile(credentialsPath))
	}
	app, err := firebase.NewApp(ctx, &firebase.Config{ProjectID: projectID}, opts...)
	if err != nil {
		return nil, fmt.Errorf("firebase app: %w", err)
	}
	client, err := app.Auth(ctx)
	if err != nil {
		return nil, fmt.Errorf("firebase auth client: %w", err)
	}
	return &FirebaseVerifier{client: client}, nil
}

// IdentityFromIDToken verifies a Firebase ID token and returns UID plus normalized phone and/or email.
func (v *FirebaseVerifier) IdentityFromIDToken(ctx context.Context, idToken string) (FirebaseIdentity, error) {
	token, err := v.client.VerifyIDToken(ctx, idToken)
	if err != nil {
		return FirebaseIdentity{}, fmt.Errorf("%w: %v", ErrInvalidFirebaseToken, err)
	}
	phone, _ := token.Claims["phone_number"].(string)
	email, _ := token.Claims["email"].(string)
	emailVerified, _ := token.Claims["email_verified"].(bool)

	phone = NormalizePhone(phone)
	email = strings.ToLower(strings.TrimSpace(email))

	if token.UID == "" || (phone == "" && email == "") {
		return FirebaseIdentity{}, ErrInvalidFirebaseToken
	}

	var authTime time.Time
	if token.AuthTime > 0 {
		authTime = time.Unix(token.AuthTime, 0).UTC()
	} else if at, ok := token.Claims["auth_time"].(float64); ok && at > 0 {
		authTime = time.Unix(int64(at), 0).UTC()
	} else if at, ok := token.Claims["auth_time"].(int64); ok && at > 0 {
		authTime = time.Unix(at, 0).UTC()
	}

	return FirebaseIdentity{
		UID:           token.UID,
		Phone:         phone,
		Email:         email,
		EmailVerified: emailVerified,
		AuthTime:      authTime,
	}, nil
}
