package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/sms"
)

const (
	OTPTTL          = 5 * time.Minute
	OTPRateWindow   = 10 * time.Minute
	OTPMaxPerWindow = 3
	OTPMaxAttempts  = 5
)

var (
	ErrRateLimited           = errors.New("otp rate limit exceeded")
	ErrInvalidOTP            = errors.New("invalid otp")
	ErrOTPExpired            = errors.New("otp expired")
	ErrOTPLocked             = errors.New("otp locked")
	ErrNoAccount             = errors.New("no account for phone")
	ErrInvalidInvite         = errors.New("invalid invite code")
	ErrTenantVacated         = errors.New("tenant vacated")
	ErrInvalidFirebaseToken  = errors.New("invalid firebase token")
	ErrFirebaseNotConfigured = errors.New("firebase auth not configured")
	ErrEmailNotVerified      = errors.New("firebase email not verified")
	ErrStaleAuthToken        = errors.New("auth token is stale: fresh re-authentication required")
	ErrRefreshTokenExpired   = errors.New("refresh token expired")
)

// FirebaseTokenVerifier validates Firebase ID tokens (implemented by FirebaseVerifier).
type FirebaseTokenVerifier interface {
	IdentityFromIDToken(ctx context.Context, idToken string) (FirebaseIdentity, error)
}

// Service handles OTP request/verify and JWT issuance.
// Rate limits live here (not in HTTP middleware).
type Service struct {
	otp         OTPRepository
	users       UserRepository
	tenants     TenantRepository
	properties  PropertyRepository
	gateway     sms.SMSGateway
	firebase    FirebaseTokenVerifier
	refreshRepo RefreshTokenRepository
	otpSecret   string
	jwtSecret   string
}

func NewService(
	otp OTPRepository,
	users UserRepository,
	tenants TenantRepository,
	properties PropertyRepository,
	gateway sms.SMSGateway,
	otpSecret, jwtSecret string,
) *Service {
	return &Service{
		otp:        otp,
		users:      users,
		tenants:    tenants,
		properties: properties,
		gateway:    gateway,
		otpSecret:  otpSecret,
		jwtSecret:  jwtSecret,
	}
}

// SetRefreshTokenRepo enables refresh token family rotation and persistence.
func (s *Service) SetRefreshTokenRepo(r RefreshTokenRepository) {
	s.refreshRepo = r
}

// SetFirebaseVerifier enables POST /auth/firebase token exchange.
func (s *Service) SetFirebaseVerifier(v FirebaseTokenVerifier) {
	s.firebase = v
}

// RequestOTP generates, stores (hashed), and SMS-sends an OTP with login copy.
// Max 3 requests per phone per 10 minutes.
func (s *Service) RequestOTP(ctx context.Context, phone string) error {
	return s.RequestOTPWithPurpose(ctx, phone, "login")
}

// RequestStepUpOTP generates, stores, and sends an OTP bound to purpose and optional batchID.
func (s *Service) RequestStepUpOTP(ctx context.Context, phone, purpose string, batchID *uuid.UUID) error {
	since := time.Now().UTC().Add(-OTPRateWindow)
	n, err := s.otp.CountRecent(ctx, phone, since)
	if err != nil {
		return fmt.Errorf("count recent otp: %w", err)
	}
	if n >= OTPMaxPerWindow {
		return ErrRateLimited
	}

	code, err := GenerateOTP()
	if err != nil {
		return err
	}

	req := &postgres.OTPRequest{
		Phone:     phone,
		OTPHash:   HashOTP(s.otpSecret, code),
		Attempts:  0,
		ExpiresAt: time.Now().UTC().Add(OTPTTL),
		Used:      false,
		Purpose:   purpose,
		BatchID:   batchID,
	}
	if err := s.otp.Create(ctx, req); err != nil {
		return fmt.Errorf("store otp: %w", err)
	}

	var msg string
	switch purpose {
	case "payout_approval":
		msg = fmt.Sprintf("Your payout approval code is %s. Valid for %d minutes.", code, int(OTPTTL.Minutes()))
	default:
		msg = fmt.Sprintf("Your login OTP is %s. Valid for %d minutes.", code, int(OTPTTL.Minutes()))
	}

	if err := s.gateway.Send(ctx, phone, msg); err != nil {
		return fmt.Errorf("send otp sms: %w", err)
	}
	return nil
}

// RequestOTPWithPurpose generates, stores (hashed), and SMS-sends an OTP tailored to a specific purpose.
// Max 3 requests per phone per 10 minutes.
func (s *Service) RequestOTPWithPurpose(ctx context.Context, phone, purpose string) error {
	return s.RequestStepUpOTP(ctx, phone, purpose, nil)
}

// VerifyStepUpOTP validates an OTP for step-up reauthentication without issuing a new JWT.
// Marks the OTP as used to prevent replay attacks.
func (s *Service) VerifyStepUpOTP(ctx context.Context, phone, otp string) error {
	return s.VerifyStepUpOTPSpecific(ctx, phone, otp, "payout_approval", nil)
}

// VerifyStepUpOTPSpecific validates an OTP bound to purpose and optional batchID.
func (s *Service) VerifyStepUpOTPSpecific(ctx context.Context, phone, otp, purpose string, batchID *uuid.UUID) error {
	req, err := s.otp.LatestUnusedByPurpose(ctx, phone, purpose, batchID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidOTP
		}
		return fmt.Errorf("load otp: %w", err)
	}

	if time.Now().UTC().After(req.ExpiresAt) {
		return ErrOTPExpired
	}
	if req.Attempts >= OTPMaxAttempts {
		return ErrOTPLocked
	}

	if !VerifyOTP(s.otpSecret, otp, req.OTPHash) {
		_ = s.otp.IncrementAttempts(ctx, req.ID)
		return ErrInvalidOTP
	}

	if err := s.otp.MarkUsed(ctx, req.ID); err != nil {
		return fmt.Errorf("mark otp used: %w", err)
	}

	return nil
}

// VerifyOTPAndIssueToken validates the OTP and returns a JWT.
// On first successful verify, creates a users row if a tenant or property owner exists for the phone.
func (s *Service) VerifyOTPAndIssueToken(ctx context.Context, phone, otp string) (token string, user *domain.User, err error) {
	req, err := s.otp.LatestUnusedByPurpose(ctx, phone, "login", nil)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil, ErrInvalidOTP
		}
		return "", nil, fmt.Errorf("load otp: %w", err)
	}

	if time.Now().UTC().After(req.ExpiresAt) {
		return "", nil, ErrOTPExpired
	}
	if req.Attempts >= OTPMaxAttempts {
		return "", nil, ErrOTPLocked
	}

	if !VerifyOTP(s.otpSecret, otp, req.OTPHash) {
		_ = s.otp.IncrementAttempts(ctx, req.ID)
		return "", nil, ErrInvalidOTP
	}

	if err := s.otp.MarkUsed(ctx, req.ID); err != nil {
		return "", nil, fmt.Errorf("mark otp used: %w", err)
	}

	return s.IssueTokenForVerifiedPhone(ctx, phone)
}

// VerifyFirebaseAndIssueToken validates a Firebase ID token and returns an app JWT.
// inviteCode, when set, lets an unknown phone or email create a pending tenant user for that property.
func (s *Service) VerifyFirebaseAndIssueToken(ctx context.Context, idToken, inviteCode string) (string, *domain.User, error) {
	if s.firebase == nil {
		return "", nil, ErrFirebaseNotConfigured
	}
	ident, err := s.firebase.IdentityFromIDToken(ctx, idToken)
	if err != nil {
		return "", nil, err
	}
	ident.Phone = NormalizePhone(ident.Phone)
	ident.Email = strings.ToLower(strings.TrimSpace(ident.Email))

	if ident.UID == "" || (ident.Phone == "" && ident.Email == "") {
		return "", nil, ErrInvalidFirebaseToken
	}

	// 1. Matched existing user by Firebase UID?
	user, err := s.users.GetByFirebaseUID(ctx, ident.UID)
	if err == nil {
		return s.finishLogin(ctx, user)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", nil, fmt.Errorf("get user by firebase uid: %w", err)
	}

	// 2. Matched existing user by phone?
	if ident.Phone != "" {
		user, err = s.users.GetByPhone(ctx, ident.Phone)
		if err == nil {
			if err := s.users.LinkFirebaseUID(ctx, user.ID, ident.UID); err != nil {
				return "", nil, fmt.Errorf("link firebase uid: %w", err)
			}
			uid := ident.UID
			user.FirebaseUID = &uid
			return s.finishLogin(ctx, user)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", nil, fmt.Errorf("get user by phone: %w", err)
		}
	}

	// 3. Matched existing user by email (only if email is verified)?
	if ident.Email != "" {
		if !ident.EmailVerified {
			return "", nil, ErrEmailNotVerified
		}
		user, err = s.users.GetByEmail(ctx, ident.Email)
		if err == nil {
			if err := s.users.LinkFirebaseUID(ctx, user.ID, ident.UID); err != nil {
				return "", nil, fmt.Errorf("link firebase uid: %w", err)
			}
			uid := ident.UID
			user.FirebaseUID = &uid
			return s.finishLogin(ctx, user)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", nil, fmt.Errorf("get user by email: %w", err)
		}
	}

	// 4. Provisioning fallback for new users
	user, err = s.createUserForIdentity(ctx, ident, inviteCode)
	if err != nil {
		return "", nil, err
	}
	return s.finishLogin(ctx, user)
}

// VerifyFirebaseStepUp validates a Firebase ID token and ensures the token's auth_time is within maxAge.
func (s *Service) VerifyFirebaseStepUp(ctx context.Context, idToken string, maxAge time.Duration) (FirebaseIdentity, error) {
	if s.firebase == nil {
		return FirebaseIdentity{}, ErrFirebaseNotConfigured
	}
	ident, err := s.firebase.IdentityFromIDToken(ctx, idToken)
	if err != nil {
		return FirebaseIdentity{}, err
	}
	if ident.UID == "" {
		return FirebaseIdentity{}, ErrInvalidFirebaseToken
	}
	if maxAge > 0 {
		if ident.AuthTime.IsZero() || time.Since(ident.AuthTime) > maxAge {
			return ident, ErrStaleAuthToken
		}
	}
	return ident, nil
}

// IssueTokenForVerifiedPhone loads or creates the user for a verified phone and issues a JWT.
func (s *Service) IssueTokenForVerifiedPhone(ctx context.Context, phone string) (string, *domain.User, error) {
	normPhone := NormalizePhone(phone)
	user, err := s.users.GetByPhone(ctx, normPhone)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", nil, fmt.Errorf("get user: %w", err)
		}
		user, err = s.createUserForIdentity(ctx, FirebaseIdentity{Phone: normPhone}, "")
		if err != nil {
			return "", nil, err
		}
	}
	return s.finishLogin(ctx, user)
}

func (s *Service) finishLogin(ctx context.Context, user *domain.User) (string, *domain.User, error) {
	if user.Role == domain.RoleTenant && user.TenantID != nil {
		tenant, terr := s.tenants.GetByID(ctx, *user.TenantID)
		if terr != nil {
			return "", nil, fmt.Errorf("get tenant for user: %w", terr)
		}
		if tenant.Status != domain.TenantStatusActive {
			return "", nil, ErrTenantVacated
		}
	}

	if err := s.users.TouchLogin(ctx, user.ID); err != nil {
		return "", nil, fmt.Errorf("touch login: %w", err)
	}
	now := time.Now().UTC()
	user.LastLoginAt = &now

	token, err := IssueToken(s.jwtSecret, user)
	if err != nil {
		return "", nil, err
	}
	return token, user, nil
}

func (s *Service) createUserForIdentity(ctx context.Context, ident FirebaseIdentity, inviteCode string) (*domain.User, error) {
	// A. Owner match by verified email (with phone backfill for anti-divergence)
	if ident.Email != "" && ident.EmailVerified {
		prop, err := s.properties.GetByOwnerEmail(ctx, ident.Email)
		if err == nil {
			pid := prop.ID
			u := &domain.User{
				Phone:      NormalizePhone(prop.OwnerPhone),
				Email:      ident.Email,
				Role:       domain.RoleOwner,
				PropertyID: &pid,
			}
			setFirebaseUID(u, ident.UID)
			if err := s.safeCreateUser(ctx, u); err != nil {
				return nil, err
			}
			return u, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get property by owner email: %w", err)
		}
	}

	// B. Owner match by phone (with email backfill for anti-divergence)
	if ident.Phone != "" {
		prop, err := s.properties.GetByOwnerPhone(ctx, ident.Phone)
		if err == nil {
			pid := prop.ID
			u := &domain.User{
				Phone:      ident.Phone,
				Email:      strings.ToLower(strings.TrimSpace(prop.OwnerEmail)),
				Role:       domain.RoleOwner,
				PropertyID: &pid,
			}
			setFirebaseUID(u, ident.UID)
			if err := s.safeCreateUser(ctx, u); err != nil {
				return nil, err
			}
			return u, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get property by owner phone: %w", err)
		}

		// C. Tenant match by phone
		tenant, err := s.tenants.GetByPhone(ctx, ident.Phone)
		if err == nil {
			if tenant.Status != domain.TenantStatusActive {
				return nil, ErrTenantVacated
			}
			tid := tenant.ID
			pid := tenant.PropertyID
			u := &domain.User{
				Phone:      ident.Phone,
				Email:      ident.Email,
				Role:       domain.RoleTenant,
				TenantID:   &tid,
				PropertyID: &pid,
			}
			setFirebaseUID(u, ident.UID)
			if err := s.safeCreateUser(ctx, u); err != nil {
				return nil, err
			}
			return u, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("get tenant by phone: %w", err)
		}
	}

	// D. Pending tenant onboarding via invite code
	code := strings.ToUpper(strings.TrimSpace(inviteCode))
	if code == "" {
		return nil, ErrNoAccount
	}
	invited, err := s.properties.GetByInviteCode(ctx, code)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidInvite
		}
		return nil, fmt.Errorf("get property by invite: %w", err)
	}
	pid := invited.ID
	u := &domain.User{
		Phone:      ident.Phone,
		Email:      ident.Email,
		Role:       domain.RoleTenant,
		PropertyID: &pid,
	}
	setFirebaseUID(u, ident.UID)
	if err := s.safeCreateUser(ctx, u); err != nil {
		return nil, fmt.Errorf("create pending tenant user: %w", err)
	}
	return u, nil
}

func (s *Service) safeCreateUser(ctx context.Context, u *domain.User) error {
	err := s.users.Create(ctx, u)
	if err == nil {
		return nil
	}

	// On insert conflict (e.g. concurrent double-click provisioning race),
	// attempt to fetch the existing user record rather than failing.
	if u.FirebaseUID != nil && *u.FirebaseUID != "" {
		if existing, ferr := s.users.GetByFirebaseUID(ctx, *u.FirebaseUID); ferr == nil {
			*u = *existing
			return nil
		}
	}
	if u.Email != "" {
		if existing, ferr := s.users.GetByEmail(ctx, u.Email); ferr == nil {
			*u = *existing
			return nil
		}
	}
	if u.Phone != "" {
		if existing, ferr := s.users.GetByPhone(ctx, u.Phone); ferr == nil {
			*u = *existing
			return nil
		}
	}

	return fmt.Errorf("create user: %w", err)
}

func setFirebaseUID(u *domain.User, firebaseUID string) {
	if firebaseUID == "" {
		return
	}
	u.FirebaseUID = &firebaseUID
}

// IssueSession creates an access token (15m) and a newly seeded refresh token family (30d).
func (s *Service) IssueSession(ctx context.Context, user *domain.User) (accessToken string, plaintextRefreshToken string, err error) {
	accessToken, err = IssueAccessToken(s.jwtSecret, user)
	if err != nil {
		return "", "", fmt.Errorf("issue access token: %w", err)
	}
	if s.refreshRepo == nil {
		return accessToken, "", nil
	}

	plaintext, hash, err := GenerateRefreshToken()
	if err != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", err)
	}

	now := time.Now().UTC()
	rt := &domain.RefreshToken{
		ID:              uuid.New(),
		UserID:          user.ID,
		FamilyID:        uuid.New(),
		TokenHash:       hash,
		ExpiresAt:       now.Add(RefreshTokenTTL),
		Revoked:         false,
		FamilyStartedAt: now,
		CreatedAt:       now,
	}
	if err := s.refreshRepo.StoreRefreshToken(ctx, rt); err != nil {
		return "", "", fmt.Errorf("store refresh token: %w", err)
	}
	return accessToken, plaintext, nil
}

// RotateRefreshToken verifies a refresh token, executes replay detection, rotates the token within the family, and returns a new access token and new refresh token.
func (s *Service) RotateRefreshToken(ctx context.Context, plaintextToken string) (newAccessToken string, newPlaintextRefreshToken string, user *domain.User, err error) {
	if s.refreshRepo == nil {
		return "", "", nil, errors.New("auth: refresh repository not configured")
	}
	trimmed := strings.TrimSpace(plaintextToken)
	if trimmed == "" {
		return "", "", nil, ErrInvalidToken
	}

	oldHash := HashRefreshToken(trimmed)

	newPlaintext, newHash, err := GenerateRefreshToken()
	if err != nil {
		return "", "", nil, fmt.Errorf("generate new refresh token: %w", err)
	}

	newRT := &domain.RefreshToken{
		TokenHash: newHash,
		ExpiresAt: time.Now().UTC().Add(RefreshTokenTTL),
	}

	rotatedRT, err := s.refreshRepo.RotateTokenTx(ctx, oldHash, newRT)
	if err != nil {
		if errors.Is(err, postgres.ErrRefreshTokenNotFound) {
			return "", "", nil, ErrInvalidToken
		}
		if errors.Is(err, postgres.ErrReplayDetected) {
			return "", "", nil, ErrReplayDetected
		}
		if errors.Is(err, postgres.ErrRefreshTokenExpired) {
			return "", "", nil, ErrRefreshTokenExpired
		}
		return "", "", nil, fmt.Errorf("rotate refresh token: %w", err)
	}

	user, err = s.users.GetByID(ctx, rotatedRT.UserID)
	if err != nil {
		return "", "", nil, fmt.Errorf("load user: %w", err)
	}

	// Verify tenant status if tenant
	if user.TenantID != nil && s.tenants != nil {
		t, err := s.tenants.GetByID(ctx, *user.TenantID)
		if err == nil && t != nil && t.Status == domain.TenantStatusVacated {
			return "", "", nil, ErrTenantVacated
		}
	}

	newAccessToken, err = IssueAccessToken(s.jwtSecret, user)
	if err != nil {
		return "", "", nil, fmt.Errorf("issue new access token: %w", err)
	}

	return newAccessToken, newPlaintext, user, nil
}

// RevokeUserSessions revokes all stored refresh tokens for a user.
func (s *Service) RevokeUserSessions(ctx context.Context, userID uuid.UUID) error {
	if s.refreshRepo != nil {
		return s.refreshRepo.RevokeUserTokens(ctx, userID)
	}
	return nil
}

// RevokeSession revokes the presented refresh token and its family.
func (s *Service) RevokeSession(ctx context.Context, plaintextToken string) error {
	if s.refreshRepo == nil {
		return nil
	}
	trimmed := strings.TrimSpace(plaintextToken)
	if trimmed == "" {
		return nil
	}
	hash := HashRefreshToken(trimmed)
	rt, err := s.refreshRepo.GetRefreshTokenByHash(ctx, hash)
	if err != nil {
		return nil
	}
	return s.refreshRepo.RevokeFamily(ctx, rt.FamilyID)
}
