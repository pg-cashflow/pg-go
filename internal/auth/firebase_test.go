package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/sms"
)

type stubFirebaseVerifier struct {
	uid   string
	phone string
	err   error
}

func (s stubFirebaseVerifier) IdentityFromIDToken(_ context.Context, _ string) (FirebaseIdentity, error) {
	if s.err != nil {
		return FirebaseIdentity{}, s.err
	}
	return FirebaseIdentity{UID: s.uid, Phone: s.phone}, nil
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
		phone: {ID: pid, OwnerPhone: phone},
	}

	svc := NewService(&stubOTP{}, users, tenants, props, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")
	svc.SetFirebaseVerifier(stubFirebaseVerifier{uid: uid, phone: phone})

	token, user, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "fake-firebase-token")
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
}

func TestVerifyFirebaseNotConfigured(t *testing.T) {
	svc := NewService(&stubOTP{}, &stubUsers{}, stubTenantsNone{}, stubProps{}, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")
	_, _, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token")
	if err != ErrFirebaseNotConfigured {
		t.Fatalf("expected ErrFirebaseNotConfigured, got %v", err)
	}
}

func TestVerifyFirebaseRejectsNoPhone(t *testing.T) {
	svc := NewService(&stubOTP{}, &stubUsers{}, stubTenantsNone{}, stubProps{}, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")
	svc.SetFirebaseVerifier(stubFirebaseVerifier{uid: "google-only", phone: ""})
	_, _, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token")
	if !errors.Is(err, ErrInvalidFirebaseToken) {
		t.Fatalf("expected ErrInvalidFirebaseToken, got %v", err)
	}
}

func TestVerifyFirebaseLinksExistingUser(t *testing.T) {
	phone := "+919999000002"
	uid := "firebase-link-1"
	existing := &domain.User{ID: uuid.New(), Phone: phone, Role: domain.RoleOwner}
	users := &stubUsers{byPhone: map[string]*domain.User{phone: existing}}
	svc := NewService(&stubOTP{}, users, stubTenantsNone{}, stubProps{}, sms.NoopGateway{}, "otp-secret", "jwt-secret-long-enough")
	svc.SetFirebaseVerifier(stubFirebaseVerifier{uid: uid, phone: phone})

	_, user, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token")
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

	_, user, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token")
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
	_, _, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "token")
	if !errors.Is(err, ErrNoAccount) {
		t.Fatalf("expected ErrNoAccount, got %v", err)
	}
}
