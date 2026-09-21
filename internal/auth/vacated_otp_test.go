package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/sms"
)

type stubOTP struct {
	req *postgres.OTPRequest
}

func (s *stubOTP) Create(context.Context, *postgres.OTPRequest) error { return nil }
func (s *stubOTP) LatestUnused(context.Context, string) (*postgres.OTPRequest, error) {
	if s.req == nil {
		return nil, pgx.ErrNoRows
	}
	return s.req, nil
}
func (s *stubOTP) IncrementAttempts(context.Context, uuid.UUID) error { return nil }
func (s *stubOTP) MarkUsed(context.Context, uuid.UUID) error          { return nil }
func (s *stubOTP) CountRecent(context.Context, string, time.Time) (int, error) {
	return 0, nil
}

type stubUsers struct {
	byPhone    map[string]*domain.User
	byEmail    map[string]*domain.User
	byFirebase map[string]*domain.User
}

func (s *stubUsers) Create(_ context.Context, u *domain.User) error {
	if s.byPhone == nil {
		s.byPhone = map[string]*domain.User{}
	}
	if s.byEmail == nil {
		s.byEmail = map[string]*domain.User{}
	}
	if s.byFirebase == nil {
		s.byFirebase = map[string]*domain.User{}
	}
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	cp := *u
	if u.FirebaseUID != nil {
		uid := *u.FirebaseUID
		cp.FirebaseUID = &uid
		s.byFirebase[uid] = &cp
	}
	if u.Phone != "" {
		s.byPhone[u.Phone] = &cp
	}
	if u.Email != "" {
		s.byEmail[strings.ToLower(u.Email)] = &cp
	}
	return nil
}

func (s *stubUsers) GetByPhone(_ context.Context, phone string) (*domain.User, error) {
	norm := NormalizePhone(phone)
	for k, u := range s.byPhone {
		if k == phone || NormalizePhone(k) == norm {
			cp := *u
			return &cp, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (s *stubUsers) GetByEmail(_ context.Context, email string) (*domain.User, error) {
	if s.byEmail == nil {
		return nil, pgx.ErrNoRows
	}
	u, ok := s.byEmail[strings.ToLower(email)]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	cp := *u
	return &cp, nil
}

func (s *stubUsers) GetByFirebaseUID(_ context.Context, firebaseUID string) (*domain.User, error) {
	u, ok := s.byFirebase[firebaseUID]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	cp := *u
	return &cp, nil
}

func (s *stubUsers) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	for _, u := range s.byPhone {
		if u.ID == id {
			cp := *u
			return &cp, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (s *stubUsers) LinkFirebaseUID(_ context.Context, userID uuid.UUID, firebaseUID string) error {
	for _, u := range s.byPhone {
		if u.ID != userID {
			continue
		}
		uid := firebaseUID
		u.FirebaseUID = &uid
		if s.byFirebase == nil {
			s.byFirebase = map[string]*domain.User{}
		}
		s.byFirebase[firebaseUID] = u
		return nil
	}
	return pgx.ErrNoRows
}

func (s *stubUsers) TouchLogin(context.Context, uuid.UUID) error { return nil }

func (s *stubUsers) IncrementTokenVersion(_ context.Context, id uuid.UUID) error {
	for _, u := range s.byPhone {
		if u.ID == id {
			if u.TokenVersion < 1 {
				u.TokenVersion = 1
			}
			u.TokenVersion++
			return nil
		}
	}
	return pgx.ErrNoRows
}

type stubTenantsAuth struct {
	byID map[uuid.UUID]*domain.Tenant
}

func (s *stubTenantsAuth) GetByID(_ context.Context, id uuid.UUID) (*domain.Tenant, error) {
	t, ok := s.byID[id]
	if !ok {
		return nil, pgx.ErrNoRows
	}
	cp := *t
	return &cp, nil
}
func (s *stubTenantsAuth) GetByPhone(context.Context, string) (*domain.Tenant, error) {
	return nil, pgx.ErrNoRows
}

func TestVerifyOTPVacatedExistingUser(t *testing.T) {
	secret := "otp-secret"
	code := "123456"
	hash := HashOTP(secret, code)
	tenantID := uuid.New()
	propID := uuid.New()
	otp := &stubOTP{req: &postgres.OTPRequest{
		ID: uuid.New(), Phone: "9999999999", OTPHash: hash,
		ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
	}}
	users := &stubUsers{byPhone: map[string]*domain.User{
		"9999999999": {
			ID: uuid.New(), Phone: "9999999999", Role: domain.RoleTenant,
			TenantID: &tenantID, PropertyID: &propID,
		},
	}}
	tenants := &stubTenantsAuth{byID: map[uuid.UUID]*domain.Tenant{
		tenantID: {ID: tenantID, PropertyID: propID, Status: domain.TenantStatusVacated},
	}}
	svc := NewService(otp, users, tenants, stubProps{}, sms.NoopGateway{}, secret, "jwt-secret-long-enough")
	_, _, err := svc.VerifyOTPAndIssueToken(context.Background(), "9999999999", code)
	if !errors.Is(err, ErrTenantVacated) {
		t.Fatalf("want ErrTenantVacated, got %v", err)
	}
}
