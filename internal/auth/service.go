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
	"github.com/pg-cashflow/pg-go/internal/invite"
	"github.com/pg-cashflow/pg-go/internal/mailer"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/sms"
)

const (
	OTPTTL            = 5 * time.Minute
	OTPRateWindow     = 10 * time.Minute
	OTPMaxPerWindow   = 3
	OTPMaxAttempts    = 5
	OTPCooldownWindow = 30 * time.Second
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
	ErrNoDeliveryChannel     = errors.New("no delivery channel configured for otp")
	ErrStaleAuthToken        = errors.New("auth token is stale: fresh re-authentication required")
	ErrRefreshTokenExpired   = errors.New("refresh token expired")
	ErrNoCurrentSession      = errors.New("cannot identify current session")
)

// FirebaseTokenVerifier validates Firebase ID tokens (implemented by FirebaseVerifier).
type FirebaseTokenVerifier interface {
	IdentityFromIDToken(ctx context.Context, idToken string) (FirebaseIdentity, error)
}

// PhoneLimiter checks rate limits per phone number before expensive provisioning operations.
type PhoneLimiter interface {
	Allow(phone string) bool
}

// Service handles OTP request/verify and JWT issuance.
// Rate limits live here (not in HTTP middleware).
type Service struct {
	otp            OTPRepository
	users          UserRepository
	tenants        TenantRepository
	properties     PropertyRepository
	gateway        sms.SMSGateway
	mailer         mailer.Mailer
	firebase       FirebaseTokenVerifier
	refreshRepo    RefreshTokenRepository
	inviteResolver InviteCodeResolver
	phoneLimiter   PhoneLimiter
	otpSecret      string
	jwtSecret      string
	cooldownWindow time.Duration
}

// InviteCodeResolver resolves property vs slot invite codes (P0-B).
type InviteCodeResolver interface {
	Resolve(ctx context.Context, code string) (*domain.InviteResolution, error)
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
		otp:            otp,
		users:          users,
		tenants:        tenants,
		properties:     properties,
		gateway:        gateway,
		otpSecret:      otpSecret,
		jwtSecret:      jwtSecret,
		cooldownWindow: OTPCooldownWindow,
	}
}

// SetCooldownWindow overrides the minimum cooldown between OTP requests (useful for testing).
func (s *Service) SetCooldownWindow(d time.Duration) {
	s.cooldownWindow = d
}

// SetMailer sets the HTML email mailer for dual-channel OTP delivery and fallback.
func (s *Service) SetMailer(m mailer.Mailer) {
	s.mailer = m
}

// SetRefreshTokenRepo enables refresh token family rotation and persistence.
func (s *Service) SetRefreshTokenRepo(r RefreshTokenRepository) {
	s.refreshRepo = r
}

// SetFirebaseVerifier enables POST /auth/firebase token exchange.
// SetInviteResolver enables slot invite code resolution during tenant provisioning.
func (s *Service) SetInviteResolver(r InviteCodeResolver) {
	s.inviteResolver = r
}

func (s *Service) SetFirebaseVerifier(v FirebaseTokenVerifier) {
	s.firebase = v
}

// SetPhoneLimiter sets the per-phone rate limiter.
func (s *Service) SetPhoneLimiter(l PhoneLimiter) {
	s.phoneLimiter = l
}

// RequestOTP generates, stores (hashed), and SMS-sends an OTP with login copy.
// Max 3 requests per phone per 10 minutes.
func (s *Service) RequestOTP(ctx context.Context, phone string) error {
	return s.RequestOTPWithPurpose(ctx, phone, "login")
}

// RequestStepUpOTPWithChannel generates, stores, and delivers an OTP via the appropriate active channel.
// Rate limiting is strictly bound to the canonical user phone identity, preventing channel-hopping resets.
func (s *Service) RequestStepUpOTPWithChannel(ctx context.Context, phone, purpose string, batchID *uuid.UUID) (string, string, error) {
	normPhone := NormalizePhone(phone)
	if normPhone != "" {
		phone = normPhone
	}
	if purpose == "" {
		purpose = "login"
	}

	// Prevent cost and enumeration abuse: for login OTP, verify account exists.
	if purpose == "login" && !s.hasAccount(ctx, phone) {
		// Return empty for anti-enumeration, skip DB insert and dispatch.
		return "", "", nil
	}

	// Canonical identity rate limit check:
	if s.cooldownWindow > 0 {
		cooldownSince := time.Now().UTC().Add(-s.cooldownWindow)
		recentCooldown, err := s.otp.CountRecentByPurpose(ctx, phone, purpose, cooldownSince)
		if err != nil {
			return "", "", fmt.Errorf("count recent otp cooldown: %w", err)
		}
		if recentCooldown > 0 {
			return "", "", ErrRateLimited
		}
	}

	since := time.Now().UTC().Add(-OTPRateWindow)
	n, err := s.otp.CountRecentByPurpose(ctx, phone, purpose, since)
	if err != nil {
		return "", "", fmt.Errorf("count recent otp: %w", err)
	}
	if n >= OTPMaxPerWindow {
		return "", "", ErrRateLimited
	}

	// Check if user has an email on file:
	var userEmail string
	if s.users != nil {
		if u, err := s.users.GetByPhone(ctx, phone); err == nil && u != nil {
			userEmail = strings.TrimSpace(u.Email)
		}
	}
	if userEmail == "" && s.properties != nil {
		if p, err := s.properties.GetByOwnerPhone(ctx, phone); err == nil && p != nil {
			userEmail = strings.TrimSpace(p.OwnerEmail)
		}
	}

	// Determine active delivery channel. Both checks follow the same
	// shape deliberately: an interface holding a Noop* value is non-nil,
	// so "!= nil" alone cannot distinguish "configured" from "discarding
	// silently" for either provider. Treating that distinction correctly
	// for SMS but not for the mailer was the root cause of REQ-SEC note
	// (silent step-up OTP loss) — keep them symmetric.
	smsAvailable := s.gateway != nil
	if _, isNoop := s.gateway.(sms.NoopGateway); isNoop {
		smsAvailable = false
	}

	mailAvailable := !mailer.IsNoop(s.mailer)

	channel := "sms"
	destination := phone

	// If SMS is not available, or purpose is payout approval and email is configured, prioritize email.
	if (!smsAvailable || purpose == "payout_approval") && userEmail != "" && mailAvailable {
		channel = "email"
		destination = userEmail
	} else if smsAvailable {
		channel = "sms"
		destination = phone
	} else if userEmail != "" && mailAvailable {
		channel = "email"
		destination = userEmail
	} else {
		// No real delivery channel configured. Fail closed loud.
		return "", "", ErrNoDeliveryChannel
	}

	code, err := GenerateOTP()
	if err != nil {
		return "", "", err
	}

	req := &postgres.OTPRequest{
		Phone:       phone,
		OTPHash:     HashOTP(s.otpSecret, code),
		Attempts:    0,
		ExpiresAt:   time.Now().UTC().Add(OTPTTL),
		Used:        false,
		Purpose:     purpose,
		BatchID:     batchID,
		Channel:     channel,
		Destination: destination,
	}
	if err := s.otp.Create(ctx, req); err != nil {
		return "", "", fmt.Errorf("store otp: %w", err)
	}

	if channel == "email" {
		subject := fmt.Sprintf("Your PG Cashflow %s code", strings.ReplaceAll(purpose, "_", " "))
		if purpose == "payout_approval" {
			subject = "Your PG Cashflow Payout Approval Code"
		}
		body := fmt.Sprintf(
			"<h2>Verification Code</h2>"+
				"<p>Your %s verification code is: <b>%s</b></p>"+
				"<p>This code is valid for %d minutes. Do not share this code with anyone.</p>",
			strings.ReplaceAll(purpose, "_", " "), code, int(OTPTTL.Minutes()),
		)
		if err := s.mailer.Send(ctx, destination, subject, body); err != nil {
			return "", "", fmt.Errorf("send otp email: %w", err)
		}
		return channel, MaskEmail(destination), nil
	}

	// SMS channel:
	var msg string
	switch purpose {
	case "payout_approval":
		msg = fmt.Sprintf("Your payout approval code is %s. Valid for %d minutes.", code, int(OTPTTL.Minutes()))
	default:
		msg = fmt.Sprintf("Your login OTP is %s. Valid for %d minutes.", code, int(OTPTTL.Minutes()))
	}

	if err := s.gateway.Send(ctx, destination, msg); err != nil {
		return "", "", fmt.Errorf("send otp sms: %w", err)
	}

	return channel, phone, nil
}

// RequestStepUpOTP generates, stores, and sends an OTP bound to purpose and optional batchID.
func (s *Service) RequestStepUpOTP(ctx context.Context, phone, purpose string, batchID *uuid.UUID) error {
	_, _, err := s.RequestStepUpOTPWithChannel(ctx, phone, purpose, batchID)
	return err
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
	normPhone := NormalizePhone(phone)
	if normPhone != "" {
		phone = normPhone
	}
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
		err := s.otp.IncrementAttempts(ctx, req.ID)
		if errors.Is(err, postgres.ErrOTPLocked) || req.Attempts+1 >= OTPMaxAttempts {
			return ErrOTPLocked
		}
		return ErrInvalidOTP
	}

	if err := s.otp.MarkUsed(ctx, req.ID); err != nil {
		if errors.Is(err, domain.ErrOTPAlreadyUsed) {
			return ErrInvalidOTP
		}
		return fmt.Errorf("mark otp used: %w", err)
	}

	return nil
}

func (s *Service) hasAccount(ctx context.Context, phone string) bool {
	if s.users == nil && s.tenants == nil && s.properties == nil {
		return true
	}
	norm := NormalizePhone(phone)
	if norm == "" {
		norm = phone
	}
	if s.users != nil {
		if _, err := s.users.GetByPhone(ctx, norm); err == nil {
			return true
		}
	}
	if s.properties != nil {
		if _, err := s.properties.GetByOwnerPhone(ctx, norm); err == nil {
			return true
		}
	}
	if s.tenants != nil {
		if t, err := s.tenants.GetByPhone(ctx, norm); err == nil && t != nil {
			if t.Status == domain.TenantStatusActive || t.Status == domain.TenantStatusPendingAllocation {
				return true
			}
		}
	}
	return false
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
		err := s.otp.IncrementAttempts(ctx, req.ID)
		if errors.Is(err, postgres.ErrOTPLocked) || req.Attempts+1 >= OTPMaxAttempts {
			return "", nil, ErrOTPLocked
		}
		return "", nil, ErrInvalidOTP
	}

	if err := s.otp.MarkUsed(ctx, req.ID); err != nil {
		if errors.Is(err, domain.ErrOTPAlreadyUsed) {
			return "", nil, ErrInvalidOTP
		}
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

	if ident.Phone != "" && s.phoneLimiter != nil && !s.phoneLimiter.Allow(ident.Phone) {
		return "", nil, ErrRateLimited
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
		switch tenant.Status {
		case domain.TenantStatusActive, domain.TenantStatusPendingAllocation:
		default:
			return "", nil, ErrTenantVacated
		}
	}

	if err := s.users.TouchLogin(ctx, user.ID); err != nil {
		return "", nil, fmt.Errorf("touch login: %w", err)
	}
	now := time.Now().UTC()
	user.LastLoginAt = &now

	token, err := IssueAccessToken(s.jwtSecret, user)
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
			if tenant.Status != domain.TenantStatusActive && tenant.Status != domain.TenantStatusPendingAllocation {
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
	var pid uuid.UUID
	if s.inviteResolver != nil {
		res, err := s.inviteResolver.Resolve(ctx, code)
		if err != nil {
			if errors.Is(err, invite.ErrInvalidInvite) {
				return nil, ErrInvalidInvite
			}
			return nil, err
		}
		pid = res.PropertyID
	} else {
		invited, err := s.properties.GetByInviteCode(ctx, code)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrInvalidInvite
			}
			return nil, fmt.Errorf("get property by invite: %w", err)
		}
		pid = invited.ID
	}
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
	return s.IssueSessionWithClient(ctx, user, "", "")
}

// IssueSessionWithClient creates an access token and binds device metadata to the refresh token family.
func (s *Service) IssueSessionWithClient(ctx context.Context, user *domain.User, userAgent, ipAddress string) (accessToken string, plaintextRefreshToken string, err error) {
	familyID := uuid.New()
	accessToken, err = IssueAccessTokenWithFamily(s.jwtSecret, user, &familyID)
	if err != nil {
		return "", "", fmt.Errorf("issue access token: %w", err)
	}
	setCachedTokenVersion(user.ID, user.TokenVersion)
	if s.refreshRepo == nil {
		return accessToken, "", nil
	}

	plaintext, hash, err := GenerateRefreshToken()
	if err != nil {
		return "", "", fmt.Errorf("generate refresh token: %w", err)
	}

	now := time.Now().UTC()
	var uaPtr, ipPtr *string
	if strings.TrimSpace(userAgent) != "" {
		trimmedUA := strings.TrimSpace(userAgent)
		uaPtr = &trimmedUA
	}
	if strings.TrimSpace(ipAddress) != "" {
		trimmedIP := strings.TrimSpace(ipAddress)
		ipPtr = &trimmedIP
	}

	rt := &domain.RefreshToken{
		ID:              uuid.New(),
		UserID:          user.ID,
		FamilyID:        familyID,
		TokenHash:       hash,
		ExpiresAt:       now.Add(RefreshTokenTTL),
		Revoked:         false,
		FamilyStartedAt: now,
		UserAgent:       uaPtr,
		IPAddress:       ipPtr,
		LastUsedAt:      now,
		CreatedAt:       now,
	}
	if err := s.refreshRepo.StoreRefreshToken(ctx, rt); err != nil {
		return "", "", fmt.Errorf("store refresh token: %w", err)
	}
	return accessToken, plaintext, nil
}

// RotateRefreshToken verifies a refresh token, executes replay detection, rotates the token within the family, and returns a new access token and new refresh token.
func (s *Service) RotateRefreshToken(ctx context.Context, plaintextToken string) (newAccessToken string, newPlaintextRefreshToken string, user *domain.User, err error) {
	return s.RotateRefreshTokenWithClient(ctx, plaintextToken, "", "")
}

// RotateRefreshTokenWithClient verifies and rotates a refresh token, refreshing its device metadata and returning a fresh access token bound to the family.
func (s *Service) RotateRefreshTokenWithClient(ctx context.Context, plaintextToken, userAgent, ipAddress string) (newAccessToken string, newPlaintextRefreshToken string, user *domain.User, err error) {
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

	var uaPtr, ipPtr *string
	if strings.TrimSpace(userAgent) != "" {
		trimmedUA := strings.TrimSpace(userAgent)
		uaPtr = &trimmedUA
	}
	if strings.TrimSpace(ipAddress) != "" {
		trimmedIP := strings.TrimSpace(ipAddress)
		ipPtr = &trimmedIP
	}

	now := time.Now().UTC()
	newRT := &domain.RefreshToken{
		TokenHash:  newHash,
		ExpiresAt:  now.Add(RefreshTokenTTL),
		UserAgent:  uaPtr,
		IPAddress:  ipPtr,
		LastUsedAt: now,
	}

	rotatedRT, err := s.refreshRepo.RotateTokenTx(ctx, oldHash, newRT)
	if err != nil {
		if errors.Is(err, domain.ErrGraceWindow) && rotatedRT != nil {
			// Multi-tab race: the token was already rotated within the last 15s.
			// The successor row is still valid. Issue a fresh access token so the
			// caller gets a new 15-minute window, but return an empty refresh-token
			// string to signal that the cookie must NOT be rotated — the browser's
			// cookie is already the correct successor plaintext.
			graceUser, uerr := s.users.GetByID(ctx, rotatedRT.UserID)
			if uerr != nil {
				return "", "", nil, fmt.Errorf("grace window: load user: %w", uerr)
			}
			if graceUser.TenantID != nil && s.tenants != nil {
				t, terr := s.tenants.GetByID(ctx, *graceUser.TenantID)
				if terr == nil && t != nil && t.Status == domain.TenantStatusVacated {
					return "", "", nil, ErrTenantVacated
				}
			}
			graceAccessToken, aerr := IssueAccessTokenWithFamily(s.jwtSecret, graceUser, &rotatedRT.FamilyID)
			if aerr != nil {
				return "", "", nil, fmt.Errorf("grace window: issue access token: %w", aerr)
			}
			setCachedTokenVersion(graceUser.ID, graceUser.TokenVersion)
			// Empty refresh-token string signals AuthRefresh to skip setRefreshCookie.
			return graceAccessToken, "", graceUser, nil
		}
		if errors.Is(err, domain.ErrRefreshTokenNotFound) {
			return "", "", nil, ErrInvalidToken
		}
		if errors.Is(err, domain.ErrReplayDetected) {
			return "", "", nil, ErrReplayDetected
		}
		if errors.Is(err, domain.ErrRefreshTokenExpired) {
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

	newAccessToken, err = IssueAccessTokenWithFamily(s.jwtSecret, user, &rotatedRT.FamilyID)
	if err != nil {
		return "", "", nil, fmt.Errorf("issue new access token: %w", err)
	}
	setCachedTokenVersion(user.ID, user.TokenVersion)

	return newAccessToken, newPlaintext, user, nil
}

// RevokeUserSessions revokes all stored refresh tokens for a user.
func (s *Service) RevokeUserSessions(ctx context.Context, userID uuid.UUID) error {
	InvalidateTokenVersionCache(userID)
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

// ListSessions returns active sessions for a user, marking the current session.
func (s *Service) ListSessions(ctx context.Context, userID uuid.UUID, currentFamilyID *uuid.UUID, currentRefreshToken string) ([]domain.ActiveSession, error) {
	if s.refreshRepo == nil {
		return nil, nil
	}
	sessions, err := s.refreshRepo.ListActiveSessions(ctx, userID)
	if err != nil {
		return nil, err
	}

	var activeFamily uuid.UUID
	if currentFamilyID != nil && *currentFamilyID != uuid.Nil {
		activeFamily = *currentFamilyID
	} else if strings.TrimSpace(currentRefreshToken) != "" {
		hash := HashRefreshToken(strings.TrimSpace(currentRefreshToken))
		if rt, err := s.refreshRepo.GetRefreshTokenByHash(ctx, hash); err == nil && rt != nil {
			activeFamily = rt.FamilyID
		}
	}

	for i := range sessions {
		if activeFamily != uuid.Nil && sessions[i].ID == activeFamily {
			sessions[i].IsCurrent = true
		}
	}

	return sessions, nil
}

// RevokeDeviceSession revokes an individual session family belonging to the user.
func (s *Service) RevokeDeviceSession(ctx context.Context, userID, familyID uuid.UUID) error {
	if s.refreshRepo == nil {
		return nil
	}
	return s.refreshRepo.RevokeFamilyByUser(ctx, userID, familyID)
}

// RevokeOtherSessions revokes all active session families belonging to the user except the caller's current session family.
func (s *Service) RevokeOtherSessions(ctx context.Context, userID uuid.UUID, currentFamilyID *uuid.UUID, currentRefreshToken string) error {
	if s.refreshRepo == nil {
		return nil
	}
	var activeFamily uuid.UUID
	if currentFamilyID != nil && *currentFamilyID != uuid.Nil {
		activeFamily = *currentFamilyID
	} else if strings.TrimSpace(currentRefreshToken) != "" {
		hash := HashRefreshToken(strings.TrimSpace(currentRefreshToken))
		if rt, err := s.refreshRepo.GetRefreshTokenByHash(ctx, hash); err == nil && rt != nil {
			activeFamily = rt.FamilyID
		}
	}
	if activeFamily == uuid.Nil {
		return ErrNoCurrentSession
	}
	return s.refreshRepo.RevokeAllExceptFamily(ctx, userID, activeFamily)
}
