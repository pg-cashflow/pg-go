package auth

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

type stubF1UserRepo struct {
	createErr      error
	byFirebaseUID  map[string]*domain.User
	byEmail        map[string]*domain.User
	byPhone        map[string]*domain.User
	byID           map[uuid.UUID]*domain.User
	getByIDCalls   atomic.Int32
	linked         map[string]uuid.UUID
	mergedInto     map[string]uuid.UUID
	identitiesByID map[uuid.UUID][]UserIdentity
}

func newStubF1UserRepo() *stubF1UserRepo {
	return &stubF1UserRepo{
		byFirebaseUID:  make(map[string]*domain.User),
		byEmail:        make(map[string]*domain.User),
		byPhone:        make(map[string]*domain.User),
		byID:           make(map[uuid.UUID]*domain.User),
		linked:         make(map[string]uuid.UUID),
		mergedInto:     make(map[string]uuid.UUID),
		identitiesByID: make(map[uuid.UUID][]UserIdentity),
	}
}

func (r *stubF1UserRepo) Create(_ context.Context, u *domain.User) error {
	if r.createErr != nil {
		return r.createErr
	}
	if u.ID == uuid.Nil {
		u.ID = uuid.New()
	}
	r.byID[u.ID] = u
	if u.FirebaseUID != nil && *u.FirebaseUID != "" {
		r.byFirebaseUID[*u.FirebaseUID] = u
	}
	return nil
}

func (r *stubF1UserRepo) GetByPhone(_ context.Context, phone string) (*domain.User, error) {
	if u, ok := r.byPhone[phone]; ok {
		return u, nil
	}
	return nil, pgx.ErrNoRows
}

func (r *stubF1UserRepo) GetByEmail(_ context.Context, email string) (*domain.User, error) {
	if u, ok := r.byEmail[email]; ok {
		return u, nil
	}
	return nil, pgx.ErrNoRows
}

func (r *stubF1UserRepo) GetByFirebaseUID(_ context.Context, uid string) (*domain.User, error) {
	if u, ok := r.byFirebaseUID[uid]; ok {
		return u, nil
	}
	return nil, pgx.ErrNoRows
}

func (r *stubF1UserRepo) GetByID(_ context.Context, id uuid.UUID) (*domain.User, error) {
	r.getByIDCalls.Add(1)
	if u, ok := r.byID[id]; ok {
		return u, nil
	}
	return nil, pgx.ErrNoRows
}

func (r *stubF1UserRepo) LinkFirebaseUID(_ context.Context, userID uuid.UUID, firebaseUID string) error {
	r.linked[firebaseUID] = userID
	return nil
}

func (r *stubF1UserRepo) TouchLogin(_ context.Context, _ uuid.UUID) error             { return nil }
func (r *stubF1UserRepo) IncrementTokenVersion(_ context.Context, _ uuid.UUID) error  { return nil }
func (r *stubF1UserRepo) SetPropertyID(_ context.Context, _, _ uuid.UUID) error       { return nil }

func (r *stubF1UserRepo) ListUserIdentities(_ context.Context, userID uuid.UUID) ([]UserIdentity, error) {
	return r.identitiesByID[userID], nil
}

func (r *stubF1UserRepo) LinkUserIdentity(_ context.Context, userID uuid.UUID, projectID, firebaseUID, provider, linkMethod string) error {
	if owner, ok := r.linked[firebaseUID]; ok && owner != userID {
		return ErrIdentityAlreadyLinked
	}
	r.linked[firebaseUID] = userID
	if u, ok := r.byID[userID]; ok {
		r.byFirebaseUID[firebaseUID] = u
	}
	r.identitiesByID[userID] = append(r.identitiesByID[userID], UserIdentity{
		UserID:            userID,
		FirebaseProjectID: projectID,
		FirebaseUID:       firebaseUID,
		Provider:          provider,
		Status:            "active",
		LinkMethod:        linkMethod,
	})
	return nil
}

func (r *stubF1UserRepo) MergeUserIdentity(_ context.Context, targetUserID, _ uuid.UUID, _, firebaseUID, _ string) error {
	r.mergedInto[firebaseUID] = targetUserID
	if u, ok := r.byID[targetUserID]; ok {
		r.byFirebaseUID[firebaseUID] = u
	}
	return nil
}

type stubFixesFirebaseVerifier struct {
	identities map[string]FirebaseIdentity
}

func (v *stubFixesFirebaseVerifier) IdentityFromIDToken(_ context.Context, idToken string) (FirebaseIdentity, error) {
	if id, ok := v.identities[idToken]; ok {
		return id, nil
	}
	return FirebaseIdentity{}, ErrInvalidFirebaseToken
}

func TestSafeCreateUser_F1_OnlyRetriesOn23505FirebaseUID(t *testing.T) {
	repo := newStubF1UserRepo()
	victim := &domain.User{ID: uuid.New(), Email: "victim@example.com", Phone: "+919876543210"}
	repo.byEmail["victim@example.com"] = victim
	repo.byPhone["+919876543210"] = victim

	// Arbitrary DB error (timeout) must NOT return victim!
	repo.createErr = errors.New("context deadline exceeded")
	svc := &Service{users: repo}
	uid := "new-uid"
	u := &domain.User{Email: "victim@example.com", Phone: "+919876543210", FirebaseUID: &uid}
	if err := svc.safeCreateUser(context.Background(), u); err == nil {
		t.Fatal("expected error on non-23505 DB failure, got nil")
	}

	// Non-firebase_uid 23505 constraint (e.g. email) must NOT log in as victim!
	repo.createErr = &pgconn.PgError{Code: "23505", ConstraintName: "users_email_key"}
	if err := svc.safeCreateUser(context.Background(), u); err == nil {
		t.Fatal("expected error on email 23505 constraint, got nil")
	}

	// firebase_uid 23505 constraint SHOULD recover the concurrent race record
	racedUser := &domain.User{ID: uuid.New(), FirebaseUID: &uid}
	repo.byFirebaseUID[uid] = racedUser
	repo.createErr = &pgconn.PgError{Code: "23505", ConstraintName: "idx_users_firebase_uid"}
	if err := svc.safeCreateUser(context.Background(), u); err != nil {
		t.Fatalf("expected 23505 firebase_uid race to recover, got: %v", err)
	}
	if u.ID != racedUser.ID {
		t.Fatalf("expected recovered user %s, got %s", racedUser.ID, u.ID)
	}
}

func TestVerifyFirebase_GoogleEmailMatchDoesNotOverwritePhoneUID(t *testing.T) {
	repo := newStubF1UserRepo()
	phoneUID := "phone-uid-1"
	existing := &domain.User{
		ID:          uuid.New(),
		Phone:       "+919876543210",
		Email:       "user@example.com",
		Role:        domain.RoleTenant,
		FirebaseUID: &phoneUID,
	}
	repo.byID[existing.ID] = existing
	repo.byFirebaseUID[phoneUID] = existing
	repo.byEmail["user@example.com"] = existing

	fb := &stubFixesFirebaseVerifier{
		identities: map[string]FirebaseIdentity{
			"google-tok": {
				UID:           "google-uid-2",
				Provider:      "google.com",
				Email:         "user@example.com",
				EmailVerified: true,
				AuthTime:      time.Now(),
			},
			"phone-tok": {
				UID:      phoneUID,
				Provider: "phone",
				Phone:    "+919876543210",
				AuthTime: time.Now(),
			},
		},
	}
	svc := &Service{
		users:     repo,
		firebase:  fb,
		otpSecret: "otp-secret-32-chars-minimum-len",
		jwtSecret: "jwt-secret-32-chars-minimum-len",
	}

	_, _, err := svc.VerifyFirebaseAndIssueToken(context.Background(), "google-tok", "")
	if !errors.Is(err, ErrIdentityMergeRequired) {
		t.Fatalf("expected ErrIdentityMergeRequired, got: %v", err)
	}

	// Explicit dual-token merge links google-uid-2 to existing user without breaking phoneUID
	if err := svc.MergeIdentity(context.Background(), existing.ID, "phone-tok", "google-tok"); err != nil {
		t.Fatalf("MergeIdentity failed: %v", err)
	}
	if repo.mergedInto["google-uid-2"] != existing.ID {
		t.Fatalf("expected google-uid-2 merged into %s", existing.ID)
	}
}

type stubF3RefreshRepo struct {
	rt *domain.RefreshToken
}

func (s *stubF3RefreshRepo) StoreRefreshToken(_ context.Context, _ *domain.RefreshToken) error { return nil }
func (s *stubF3RefreshRepo) GetRefreshTokenByHash(_ context.Context, _ string) (*domain.RefreshToken, error) {
	return s.rt, nil
}
func (s *stubF3RefreshRepo) RevokeRefreshToken(_ context.Context, _ uuid.UUID) error           { return nil }
func (s *stubF3RefreshRepo) RevokeFamily(_ context.Context, _ uuid.UUID) error                 { return nil }
func (s *stubF3RefreshRepo) RevokeFamilyByUser(_ context.Context, _, _ uuid.UUID) error        { return nil }
func (s *stubF3RefreshRepo) RevokeAllExceptFamily(_ context.Context, _, _ uuid.UUID) error     { return nil }
func (s *stubF3RefreshRepo) RevokeUserTokens(_ context.Context, _ uuid.UUID) error             { return nil }
func (s *stubF3RefreshRepo) ListActiveSessions(_ context.Context, _ uuid.UUID) ([]domain.ActiveSession, error) {
	return nil, nil
}
func (s *stubF3RefreshRepo) RotateTokenTx(_ context.Context, _ string, newRT *domain.RefreshToken) (*domain.RefreshToken, error) {
	newRT.UserID = s.rt.UserID
	newRT.FamilyID = s.rt.FamilyID
	return newRT, nil
}
func (s *stubF3RefreshRepo) PurgeExpiredTokens(_ context.Context, _ time.Duration) (int64, error) {
	return 0, nil
}

func TestRefreshSession_F3_SingleUserLookup(t *testing.T) {
	repo := newStubF1UserRepo()
	uid := uuid.New()
	repo.byID[uid] = &domain.User{ID: uid, Role: domain.RoleOwner, TokenVersion: 1}
	rtRepo := &stubF3RefreshRepo{
		rt: &domain.RefreshToken{UserID: uid, FamilyID: uuid.New()},
	}
	svc := &Service{
		users:       repo,
		refreshRepo: rtRepo,
		otpSecret:   "otp-secret-32-chars-minimum-len",
		jwtSecret:   "jwt-secret-32-chars-minimum-len",
	}
	_, _, u, err := svc.RotateRefreshTokenWithClient(context.Background(), "plain-refresh", "ua", "127.0.0.1")
	if err != nil {
		t.Fatalf("RefreshSession failed: %v", err)
	}
	if u == nil || u.ID != uid {
		t.Fatalf("unexpected user: %+v", u)
	}
	if calls := repo.getByIDCalls.Load(); calls != 1 {
		t.Fatalf("expected exactly 1 users.GetByID call during refresh (F3), got %d", calls)
	}
}

func TestIsValidIndianMobile_F5(t *testing.T) {
	valid := []string{"9876543210", "6000000000", "7123456789", "8989898989", "+919876543210", "09876543210"}
	for _, p := range valid {
		if !IsValidIndianMobile(p) {
			t.Errorf("expected IsValidIndianMobile(%q) = true", p)
		}
	}
	invalid := []string{"0123456789", "1234567890", "5999999999", "98765", "+14155552671"}
	for _, p := range invalid {
		if IsValidIndianMobile(p) {
			t.Errorf("expected IsValidIndianMobile(%q) = false", p)
		}
	}
}
