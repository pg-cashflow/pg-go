package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/sms"
)

type stubFirebaseVerifier struct {
	uid           string
	phone         string
	email         string
	emailVerified bool
	err           error
}

func (s stubFirebaseVerifier) IdentityFromIDToken(_ context.Context, _ string) (FirebaseIdentity, error) {
	if s.err != nil {
		return FirebaseIdentity{}, s.err
	}
	return FirebaseIdentity{
		UID:           s.uid,
		Phone:         s.phone,
		Email:         s.email,
		EmailVerified: s.emailVerified,
	}, nil
}

type stubPropsByOwnerPhone map[string]*domain.Property

func (m stubPropsByOwnerPhone) GetByOwnerPhone(_ context.Context, phone string) (*domain.Property, error) {
	p, ok := m[phone]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	cp := *p
	return &cp, nil
}

func (m stubPropsByOwnerPhone) GetByOwnerEmail(_ context.Context, email string) (*domain.Property, error) {
	clean := strings.ToLower(strings.TrimSpace(email))
	for _, p := range m {
		if strings.ToLower(strings.TrimSpace(p.OwnerEmail)) == clean && clean != "" {
			cp := *p
			return &cp, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (m stubPropsByOwnerPhone) GetByInviteCode(_ context.Context, code string) (*domain.Property, error) {
	for _, p := range m {
		if p.InviteCode == code && code != "" {
			cp := *p
			return &cp, nil
		}
	}
	return nil, pgx.ErrNoRows
}

type stubProps struct {
	byPhone map[string]*domain.Property
	byEmail map[string]*domain.Property
	byCode  map[string]*domain.Property
}

func (m stubProps) GetByOwnerPhone(_ context.Context, phone string) (*domain.Property, error) {
	if m.byPhone != nil {
		if p, ok := m.byPhone[phone]; ok {
			cp := *p
			return &cp, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (m stubProps) GetByOwnerEmail(_ context.Context, email string) (*domain.Property, error) {
	if m.byEmail != nil {
		if p, ok := m.byEmail[strings.ToLower(strings.TrimSpace(email))]; ok {
			cp := *p
			return &cp, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (m stubProps) GetByInviteCode(_ context.Context, code string) (*domain.Property, error) {
	if m.byCode != nil {
		if p, ok := m.byCode[code]; ok {
			cp := *p
			return &cp, nil
		}
	}
	return nil, pgx.ErrNoRows
}

type stubTenantsNone struct{}

func (stubTenantsNone) GetByID(context.Context, uuid.UUID) (*domain.Tenant, error) {
	return nil, pgx.ErrNoRows
}
func (stubTenantsNone) GetByPhone(context.Context, string) (*domain.Tenant, error) {
	return nil, pgx.ErrNoRows
}

func TestVerifyFirebaseAndIssueTokenOwner(t *testing.T) {
	phone := "+919999000001"
	uid := "firebase-owner-1"
	pid := uuid.New()
	users := &stubUsers{byPhone: map[string]*domain.User{}}
	tenants := stubTenantsNone{}
	props := stubPropsByOwnerPhone{
		phone: {ID: pid, OwnerPhone: phone, OwnerEmail: "owner@test.com"},
	}

	svc := NewService(&stubOTP{}, users, tenants, props, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")
	svc.SetFirebaseVerifier(stubFirebaseVerifier{uid: uid, phone: phone})

	token, user, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "fake-firebase-token", "")
	if err != nil {
		t.Fatalf("VerifyFirebaseAndIssueToken: %v", err)
	}
	if token == "" {
		t.Fatal("expected token")
	}
	if user == nil || user.Role != domain.RoleOwner {
		t.Fatalf("expected owner user, got %+v", user)
	}
	if user.FirebaseUID == nil || *user.FirebaseUID != uid {
		t.Fatalf("expected firebase uid %s, got %+v", uid, user.FirebaseUID)
	}
	if user.Email != "owner@test.com" {
		t.Fatalf("expected backfilled email owner@test.com, got %s", user.Email)
	}
}

func TestVerifyFirebaseNotConfigured(t *testing.T) {
	svc := NewService(&stubOTP{}, &stubUsers{}, stubTenantsNone{}, stubProps{}, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")
	_, _, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token", "")
	if err != ErrFirebaseNotConfigured {
		t.Fatalf("expected ErrFirebaseNotConfigured, got %v", err)
	}
}

func TestVerifyFirebaseRejectsNoPhoneAndNoEmail(t *testing.T) {
	svc := NewService(&stubOTP{}, &stubUsers{}, stubTenantsNone{}, stubProps{}, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")
	svc.SetFirebaseVerifier(stubFirebaseVerifier{uid: "empty-claims", phone: "", email: ""})
	_, _, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token", "")
	if !errors.Is(err, ErrInvalidFirebaseToken) {
		t.Fatalf("expected ErrInvalidFirebaseToken, got %v", err)
	}
}

func TestVerifyFirebaseGoogleOwnerEmailMatch(t *testing.T) {
	email := "Owner.Test@GMAIL.com"
	storedPhone := "9876543210" // bare 10-digit Indian number in DB
	uid := "google-owner-uid-1"
	pid := uuid.New()

	users := &stubUsers{byPhone: map[string]*domain.User{}, byEmail: map[string]*domain.User{}}
	props := stubPropsByOwnerPhone{
		"+919876543210": {ID: pid, OwnerPhone: storedPhone, OwnerEmail: "owner.test@gmail.com"},
	}

	svc := NewService(&stubOTP{}, users, stubTenantsNone{}, props, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")
	svc.SetFirebaseVerifier(stubFirebaseVerifier{
		uid:           uid,
		email:         email,
		emailVerified: true,
	})

	token, user, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "fake-google-token", "")
	if err != nil {
		t.Fatalf("Google login failed: %v", err)
	}
	if token == "" {
		t.Fatal("expected JWT token")
	}
	if user.Role != domain.RoleOwner {
		t.Fatalf("expected role owner, got %v", user.Role)
	}
	if user.Email != "owner.test@gmail.com" {
		t.Fatalf("expected normalized email, got %s", user.Email)
	}
	// Verify phone was normalized to E.164 upon backfill
	if user.Phone != "+919876543210" {
		t.Fatalf("expected E.164 backfilled phone +919876543210, got %s", user.Phone)
	}
}

func TestVerifyFirebaseConvergencePhoneAndGoogle(t *testing.T) {
	phone := "+919876543210"
	email := "converge@test.com"
	pid := uuid.New()
	phoneUID := "uid-phone-login"
	googleUID := "uid-google-login"

	users := &stubUsers{byPhone: map[string]*domain.User{}, byEmail: map[string]*domain.User{}}
	props := stubPropsByOwnerPhone{
		phone: {ID: pid, OwnerPhone: phone, OwnerEmail: email},
	}

	svc := NewService(&stubOTP{}, users, stubTenantsNone{}, props, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")

	// Step 1: Owner logs in via Phone OTP
	svc.SetFirebaseVerifier(stubFirebaseVerifier{uid: phoneUID, phone: phone})
	_, u1, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token1", "")
	if err != nil {
		t.Fatalf("phone login: %v", err)
	}

	// Step 2: Owner logs in later via Google (same email)
	svc.SetFirebaseVerifier(stubFirebaseVerifier{uid: googleUID, email: email, emailVerified: true})
	_, u2, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token2", "")
	if err != nil {
		t.Fatalf("google login: %v", err)
	}

	// Step 3: Verify both resolved to the EXACT SAME user record (anti-divergence)
	if u1.ID != u2.ID {
		t.Fatalf("divergence! User 1 id=%s != User 2 id=%s", u1.ID, u2.ID)
	}
	if u2.FirebaseUID == nil || *u2.FirebaseUID != googleUID {
		t.Fatalf("expected updated firebase uid %s, got %+v", googleUID, u2.FirebaseUID)
	}
}

func TestVerifyFirebaseRejectsUnverifiedEmail(t *testing.T) {
	users := &stubUsers{}
	props := stubPropsByOwnerPhone{
		"+919876543210": {ID: uuid.New(), OwnerPhone: "+919876543210", OwnerEmail: "unverified@test.com"},
	}

	svc := NewService(&stubOTP{}, users, stubTenantsNone{}, props, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")
	svc.SetFirebaseVerifier(stubFirebaseVerifier{
		uid:           "unverified-uid",
		email:         "unverified@test.com",
		emailVerified: false,
	})

	_, _, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token", "")
	if !errors.Is(err, ErrEmailNotVerified) {
		t.Fatalf("expected ErrEmailNotVerified, got %v", err)
	}
}

func TestMultiPropertyOwnerConsistentBinding(t *testing.T) {
	phone := "+919999000005"
	email := "multi@owner.com"
	pid1 := uuid.New()

	users := &stubUsers{}
	// First created property
	props := stubPropsByOwnerPhone{
		phone: {ID: pid1, OwnerPhone: phone, OwnerEmail: email},
	}

	svc := NewService(&stubOTP{}, users, stubTenantsNone{}, props, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")

	// Phone login
	svc.SetFirebaseVerifier(stubFirebaseVerifier{uid: "uid-phone", phone: phone})
	_, uPhone, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token1", "")
	if err != nil {
		t.Fatalf("phone login: %v", err)
	}

	// Google login
	svc.SetFirebaseVerifier(stubFirebaseVerifier{uid: "uid-google", email: email, emailVerified: true})
	_, uGoogle, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token2", "")
	if err != nil {
		t.Fatalf("google login: %v", err)
	}

	if *uPhone.PropertyID != pid1 || *uGoogle.PropertyID != pid1 {
		t.Fatalf("inconsistent property binding: phone=%v, google=%v, expected=%v", uPhone.PropertyID, uGoogle.PropertyID, pid1)
	}
}

type conflictUsersStub struct {
	stubUsers
	attempts int
}

func (c *conflictUsersStub) Create(ctx context.Context, u *domain.User) error {
	c.attempts++
	if c.attempts == 1 {
		// First attempt creates and commits user in background
		_ = c.stubUsers.Create(ctx, u)
		return nil
	}
	// Simulated concurrent insert: duplicate key error
	return errors.New("duplicate key value violates unique constraint")
}

func TestVerifyFirebaseProvisioningInsertConflictEmail(t *testing.T) {
	email := "race-email@test.com"
	pid := uuid.New()
	props := stubPropsByOwnerPhone{
		"+919876543210": {ID: pid, OwnerPhone: "+919876543210", OwnerEmail: email},
	}
	users := &conflictUsersStub{stubUsers: stubUsers{}}
	svc := NewService(&stubOTP{}, users, stubTenantsNone{}, props, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")
	svc.SetFirebaseVerifier(stubFirebaseVerifier{uid: "race-uid", email: email, emailVerified: true})

	_, u1, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token1", "")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	u2 := &domain.User{Email: email}
	err = svc.safeCreateUser(context.Background(), u2)
	if err != nil {
		t.Fatalf("safeCreateUser failed on email conflict: %v", err)
	}
	if u2.ID != u1.ID {
		t.Fatalf("expected resolved user id %s, got %s", u1.ID, u2.ID)
	}
}

func TestVerifyFirebaseProvisioningInsertConflictPhone(t *testing.T) {
	phone := "+919876543211"
	pid := uuid.New()
	props := stubPropsByOwnerPhone{
		phone: {ID: pid, OwnerPhone: phone, OwnerEmail: "owner-phone@test.com"},
	}
	users := &conflictUsersStub{stubUsers: stubUsers{}}
	svc := NewService(&stubOTP{}, users, stubTenantsNone{}, props, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")
	svc.SetFirebaseVerifier(stubFirebaseVerifier{uid: "race-phone-uid", phone: phone})

	_, u1, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token1", "")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}

	u2 := &domain.User{Phone: phone}
	err = svc.safeCreateUser(context.Background(), u2)
	if err != nil {
		t.Fatalf("safeCreateUser failed on phone conflict: %v", err)
	}
	if u2.ID != u1.ID {
		t.Fatalf("expected resolved user id %s, got %s", u1.ID, u2.ID)
	}
}

func TestVerifyFirebaseProvisioningInsertConflictUID(t *testing.T) {
	uid := "race-uid-direct"
	users := &conflictUsersStub{stubUsers: stubUsers{}, attempts: 1}
	svc := NewService(&stubOTP{}, users, stubTenantsNone{}, stubProps{}, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")

	existing := &domain.User{ID: uuid.New(), FirebaseUID: &uid, Phone: "+919876543212"}
	_ = users.stubUsers.Create(context.Background(), existing)

	u2 := &domain.User{FirebaseUID: &uid}
	err := svc.safeCreateUser(context.Background(), u2)
	if err != nil {
		t.Fatalf("safeCreateUser failed on UID conflict: %v", err)
	}
	if u2.ID != existing.ID {
		t.Fatalf("expected resolved user id %s, got %s", existing.ID, u2.ID)
	}
}

func TestVerifyFirebaseLinksExistingUser(t *testing.T) {
	phone := "+919999000002"
	uid := "firebase-link-1"
	existing := &domain.User{ID: uuid.New(), Phone: phone, Role: domain.RoleOwner}
	users := &stubUsers{byPhone: map[string]*domain.User{phone: existing}}
	svc := NewService(&stubOTP{}, users, stubTenantsNone{}, stubProps{}, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")
	svc.SetFirebaseVerifier(stubFirebaseVerifier{uid: uid, phone: phone})

	_, user, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token", "")
	if err != nil {
		t.Fatalf("VerifyFirebaseAndIssueToken: %v", err)
	}
	if user.ID != existing.ID {
		t.Fatalf("expected existing user %s, got %s", existing.ID, user.ID)
	}
	if user.FirebaseUID == nil || *user.FirebaseUID != uid {
		t.Fatalf("expected linked firebase uid %s, got %+v", uid, user.FirebaseUID)
	}
}

func TestVerifyFirebaseFindsByUID(t *testing.T) {
	phone := "+919999000003"
	uid := "firebase-existing-uid"
	existing := &domain.User{ID: uuid.New(), Phone: phone, Role: domain.RoleOwner, FirebaseUID: &uid}
	users := &stubUsers{
		byPhone:    map[string]*domain.User{phone: existing},
		byFirebase: map[string]*domain.User{uid: existing},
	}
	svc := NewService(&stubOTP{}, users, stubTenantsNone{}, stubProps{}, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")
	svc.SetFirebaseVerifier(stubFirebaseVerifier{uid: uid, phone: phone})

	_, user, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token", "")
	if err != nil {
		t.Fatalf("VerifyFirebaseAndIssueToken: %v", err)
	}
	if user.ID != existing.ID {
		t.Fatalf("expected user %s, got %s", existing.ID, user.ID)
	}
}

func TestVerifyFirebaseNoAccount(t *testing.T) {
	svc := NewService(&stubOTP{}, &stubUsers{}, stubTenantsNone{}, stubProps{}, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")
	svc.SetFirebaseVerifier(stubFirebaseVerifier{uid: "firebase-unknown", phone: "+919999000099"})
	_, _, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token", "")
	if !errors.Is(err, ErrNoAccount) {
		t.Fatalf("expected ErrNoAccount, got %v", err)
	}
}

func TestVerifyFirebaseInviteCreatesPendingTenant(t *testing.T) {
	phone := "+919999000088"
	pid := uuid.New()
	props := stubPropsByOwnerPhone{
		"owner-not-this": {ID: pid, InviteCode: "ABCD1234"},
	}
	users := &stubUsers{byPhone: map[string]*domain.User{}}
	svc := NewService(&stubOTP{}, users, stubTenantsNone{}, props, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")
	svc.SetFirebaseVerifier(stubFirebaseVerifier{uid: "firebase-join", phone: phone})

	_, user, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token", "abcd1234")
	if err != nil {
		t.Fatalf("invite join: %v", err)
	}
	if user.Role != domain.RoleTenant || user.TenantID != nil {
		t.Fatalf("expected pending tenant, got %+v", user)
	}
	if user.PropertyID == nil || *user.PropertyID != pid {
		t.Fatalf("expected property %s, got %+v", pid, user.PropertyID)
	}
}

func TestVerifyFirebaseBadInvite(t *testing.T) {
	svc := NewService(&stubOTP{}, &stubUsers{}, stubTenantsNone{}, stubProps{}, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")
	svc.SetFirebaseVerifier(stubFirebaseVerifier{uid: "firebase-bad", phone: "+919999000077"})
	_, _, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token", "NOPE0000")
	if !errors.Is(err, ErrInvalidInvite) {
		t.Fatalf("expected ErrInvalidInvite, got %v", err)
	}
}
