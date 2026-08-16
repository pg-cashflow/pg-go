package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
)

// FirebaseTokenVerifier validates Firebase ID tokens (implemented by FirebaseVerifier).
type FirebaseTokenVerifier interface {
	IdentityFromIDToken(ctx context.Context, idToken string) (FirebaseIdentity, error)
}

// Service handles OTP request/verify and JWT issuance.
// Rate limits live here (not in HTTP middleware).
type Service struct {
	otp        OTPRepository
	users      UserRepository
	tenants    TenantRepository
	properties PropertyRepository
	gateway    sms.SMSGateway
	firebase   FirebaseTokenVerifier
	otpSecret  string
	jwtSecret  string
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

// SetFirebaseVerifier enables POST /auth/firebase token exchange.
func (s *Service) SetFirebaseVerifier(v FirebaseTokenVerifier) {
	s.firebase = v
}

// RequestOTP generates, stores (hashed), and SMS-sends an OTP.
// Max 3 requests per phone per 10 minutes.
func (s *Service) RequestOTP(ctx context.Context, phone string) error {
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
	}
	if err := s.otp.Create(ctx, req); err != nil {
		return fmt.Errorf("store otp: %w", err)
	}

	msg := fmt.Sprintf("Your login OTP is %s. Valid for %d minutes.", code, int(OTPTTL.Minutes()))
	if err := s.gateway.Send(ctx, phone, msg); err != nil {
		return fmt.Errorf("send otp sms: %w", err)
	}
	return nil
}

// VerifyOTPAndIssueToken validates the OTP and returns a JWT.
// On first successful verify, creates a users row if a tenant or property owner exists for the phone.
func (s *Service) VerifyOTPAndIssueToken(ctx context.Context, phone, otp string) (token string, user *domain.User, err error) {
	req, err := s.otp.LatestUnused(ctx, phone)
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
// inviteCode, when set, lets an unknown phone create a pending tenant user for that property.
func (s *Service) VerifyFirebaseAndIssueToken(ctx context.Context, idToken, inviteCode string) (string, *domain.User, error) {
	if s.firebase == nil {
		return "", nil, ErrFirebaseNotConfigured
	}
	ident, err := s.firebase.IdentityFromIDToken(ctx, idToken)
	if err != nil {
		return "", nil, err
	}
	if ident.UID == "" || ident.Phone == "" {
		return "", nil, ErrInvalidFirebaseToken
	}

	user, err := s.users.GetByFirebaseUID(ctx, ident.UID)
	if err == nil {
		return s.finishLogin(ctx, user)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", nil, fmt.Errorf("get user by firebase uid: %w", err)
	}

	user, err = s.users.GetByPhone(ctx, ident.Phone)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", nil, fmt.Errorf("get user: %w", err)
		}
		user, err = s.createUserForPhone(ctx, ident.Phone, ident.UID, inviteCode)
		if err != nil {
			return "", nil, err
		}
		return s.finishLogin(ctx, user)
	}

	if err := s.users.LinkFirebaseUID(ctx, user.ID, ident.UID); err != nil {
		return "", nil, fmt.Errorf("link firebase uid: %w", err)
	}
	uid := ident.UID
	user.FirebaseUID = &uid
	return s.finishLogin(ctx, user)
}

// IssueTokenForVerifiedPhone loads or creates the user for a verified phone and issues a JWT.
func (s *Service) IssueTokenForVerifiedPhone(ctx context.Context, phone string) (string, *domain.User, error) {
	user, err := s.users.GetByPhone(ctx, phone)
	if err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return "", nil, fmt.Errorf("get user: %w", err)
		}
		user, err = s.createUserForPhone(ctx, phone, "", "")
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

func (s *Service) createUserForPhone(ctx context.Context, phone, firebaseUID, inviteCode string) (*domain.User, error) {
	tenant, err := s.tenants.GetByPhone(ctx, phone)
	if err == nil {
		if tenant.Status != domain.TenantStatusActive {
			return nil, ErrTenantVacated
		}
		tid := tenant.ID
		pid := tenant.PropertyID
		u := &domain.User{
			Phone:      phone,
			Role:       domain.RoleTenant,
			TenantID:   &tid,
			PropertyID: &pid,
		}
		setFirebaseUID(u, firebaseUID)
		if err := s.users.Create(ctx, u); err != nil {
			return nil, fmt.Errorf("create tenant user: %w", err)
		}
		return u, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("get tenant: %w", err)
	}

	prop, err := s.properties.GetByOwnerPhone(ctx, phone)
	if err == nil {
		pid := prop.ID
		u := &domain.User{
			Phone:      phone,
			Role:       domain.RoleOwner,
			PropertyID: &pid,
		}
		setFirebaseUID(u, firebaseUID)
		if err := s.users.Create(ctx, u); err != nil {
			return nil, fmt.Errorf("create owner user: %w", err)
		}
		return u, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("get property by owner phone: %w", err)
	}

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
		Phone:      phone,
		Role:       domain.RoleTenant,
		PropertyID: &pid,
	}
	setFirebaseUID(u, firebaseUID)
	if err := s.users.Create(ctx, u); err != nil {
		return nil, fmt.Errorf("create pending tenant user: %w", err)
	}
	return u, nil
}

func setFirebaseUID(u *domain.User, firebaseUID string) {
	if firebaseUID == "" {
		return
	}
	u.FirebaseUID = &firebaseUID
}
