package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type memoryRefreshTokenRepo struct {
	mu     sync.Mutex
	tokens map[string]*domain.RefreshToken
	byID   map[uuid.UUID]*domain.RefreshToken
}

func newMemoryRefreshTokenRepo() *memoryRefreshTokenRepo {
	return &memoryRefreshTokenRepo{
		tokens: make(map[string]*domain.RefreshToken),
		byID:   make(map[uuid.UUID]*domain.RefreshToken),
	}
}

func (m *memoryRefreshTokenRepo) StoreRefreshToken(ctx context.Context, rt *domain.RefreshToken) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokens[rt.TokenHash] = rt
	m.byID[rt.ID] = rt
	return nil
}

func (m *memoryRefreshTokenRepo) GetRefreshTokenByHash(ctx context.Context, hash string) (*domain.RefreshToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rt, ok := m.tokens[hash]
	if !ok {
		return nil, postgres.ErrRefreshTokenNotFound
	}
	return rt, nil
}

func (m *memoryRefreshTokenRepo) RevokeRefreshToken(ctx context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rt, ok := m.byID[id]; ok {
		rt.Revoked = true
		now := time.Now()
		rt.RevokedAt = &now
	}
	return nil
}

func (m *memoryRefreshTokenRepo) RevokeFamily(ctx context.Context, familyID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for _, rt := range m.tokens {
		if rt.FamilyID == familyID {
			rt.Revoked = true
			if rt.RevokedAt == nil {
				rt.RevokedAt = &now
			}
		}
	}
	return nil
}

func (m *memoryRefreshTokenRepo) RevokeUserTokens(ctx context.Context, userID uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	for _, rt := range m.tokens {
		if rt.UserID == userID {
			rt.Revoked = true
			if rt.RevokedAt == nil {
				rt.RevokedAt = &now
			}
		}
	}
	return nil
}

func (m *memoryRefreshTokenRepo) RotateTokenTx(ctx context.Context, oldHash string, newRT *domain.RefreshToken) (*domain.RefreshToken, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	oldRT, ok := m.tokens[oldHash]
	if !ok {
		return nil, postgres.ErrRefreshTokenNotFound
	}

	if oldRT.Revoked {
		// Check grace window
		if oldRT.RevokedAt != nil && time.Since(*oldRT.RevokedAt) <= 30*time.Second {
			for _, rt := range m.tokens {
				if rt.FamilyID == oldRT.FamilyID && !rt.Revoked && time.Now().Before(rt.ExpiresAt) {
					return rt, nil
				}
			}
		}
		// Grace window exceeded or replacement invalid -> revoke family
		now := time.Now()
		for _, rt := range m.tokens {
			if rt.FamilyID == oldRT.FamilyID {
				rt.Revoked = true
				if rt.RevokedAt == nil {
					rt.RevokedAt = &now
				}
			}
		}
		return nil, postgres.ErrReplayDetected
	}

	if time.Now().After(oldRT.ExpiresAt) {
		return nil, postgres.ErrRefreshTokenExpired
	}

	newRT.ID = uuid.New()
	newRT.UserID = oldRT.UserID
	newRT.FamilyID = oldRT.FamilyID
	newRT.CreatedAt = time.Now()
	newRT.Revoked = false

	now := time.Now()
	oldRT.Revoked = true
	oldRT.RevokedAt = &now

	m.tokens[newRT.TokenHash] = newRT
	m.byID[newRT.ID] = newRT

	return newRT, nil
}

func (m *memoryRefreshTokenRepo) PurgeExpiredTokens(ctx context.Context, olderThan time.Duration) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cutoff := time.Now().Add(-olderThan)
	var count int64
	for hash, rt := range m.tokens {
		if rt.ExpiresAt.Before(cutoff) {
			delete(m.tokens, hash)
			delete(m.byID, rt.ID)
			count++
		}
	}
	return count, nil
}

type memoryUserRepo struct {
	users map[uuid.UUID]*domain.User
}

func (u *memoryUserRepo) GetByID(ctx context.Context, id uuid.UUID) (*domain.User, error) {
	usr, ok := u.users[id]
	if !ok {
		return nil, ErrNoAccount
	}
	return usr, nil
}

func (u *memoryUserRepo) Create(ctx context.Context, user *domain.User) error {
	u.users[user.ID] = user
	return nil
}

func (u *memoryUserRepo) GetByPhone(ctx context.Context, phone string) (*domain.User, error) {
	for _, usr := range u.users {
		if usr.Phone == phone {
			return usr, nil
		}
	}
	return nil, ErrNoAccount
}

func (u *memoryUserRepo) GetByEmail(ctx context.Context, email string) (*domain.User, error) {
	for _, usr := range u.users {
		if usr.Email == email {
			return usr, nil
		}
	}
	return nil, ErrNoAccount
}

func (u *memoryUserRepo) GetByFirebaseUID(ctx context.Context, uid string) (*domain.User, error) {
	for _, usr := range u.users {
		if usr.FirebaseUID != nil && *usr.FirebaseUID == uid {
			return usr, nil
		}
	}
	return nil, ErrNoAccount
}

func (u *memoryUserRepo) Update(ctx context.Context, user *domain.User) error {
	u.users[user.ID] = user
	return nil
}

func (u *memoryUserRepo) Delete(ctx context.Context, id uuid.UUID) error {
	delete(u.users, id)
	return nil
}

func (u *memoryUserRepo) List(ctx context.Context) ([]*domain.User, error) {
	var list []*domain.User
	for _, usr := range u.users {
		list = append(list, usr)
	}
	return list, nil
}

func (u *memoryUserRepo) IncrementTokenVersion(ctx context.Context, id uuid.UUID) error {
	if usr, ok := u.users[id]; ok {
		usr.TokenVersion++
	}
	return nil
}

func (u *memoryUserRepo) LinkFirebaseUID(ctx context.Context, userID uuid.UUID, firebaseUID string) error {
	if usr, ok := u.users[userID]; ok {
		usr.FirebaseUID = &firebaseUID
	}
	return nil
}

func (u *memoryUserRepo) TouchLogin(ctx context.Context, id uuid.UUID) error {
	return nil
}

func TestRefreshTokenFamilyRotation(t *testing.T) {
	ctx := context.Background()
	refreshRepo := newMemoryRefreshTokenRepo()
	userRepo := &memoryUserRepo{users: make(map[uuid.UUID]*domain.User)}

	user := &domain.User{
		ID:           uuid.New(),
		Phone:        "+919876543210",
		Role:         domain.RoleOwner,
		TokenVersion: 1,
	}
	_ = userRepo.Create(ctx, user)

	svc := NewService(nil, userRepo, nil, nil, nil, "otp-secret", "jwt-secret-very-secure-1234567890")
	svc.SetRefreshTokenRepo(refreshRepo)

	// Step 1: Initial Session Issuance
	accToken1, refToken1, err := svc.IssueSession(ctx, user)
	if err != nil {
		t.Fatalf("unexpected error issuing session: %v", err)
	}
	if accToken1 == "" || refToken1 == "" {
		t.Fatalf("expected non-empty access and refresh tokens")
	}

	// Verify claims of access token (TTL ~15 minutes)
	claims1, err := VerifyToken("jwt-secret-very-secure-1234567890", accToken1)
	if err != nil {
		t.Fatalf("verify access token 1 failed: %v", err)
	}
	if claims1.UserID != user.ID {
		t.Errorf("expected user id %s, got %s", user.ID, claims1.UserID)
	}

	// Step 2: First Rotation (refToken1 -> refToken2)
	accToken2, refToken2, rotatedUser, err := svc.RotateRefreshToken(ctx, refToken1)
	if err != nil {
		t.Fatalf("unexpected error rotating refresh token: %v", err)
	}
	if refToken2 == refToken1 {
		t.Errorf("expected new refresh token string after rotation")
	}
	if rotatedUser.ID != user.ID {
		t.Errorf("expected user %s, got %s", user.ID, rotatedUser.ID)
	}

	// Step 3: Second Rotation (refToken2 -> refToken3)
	accToken3, refToken3, _, err := svc.RotateRefreshToken(ctx, refToken2)
	if err != nil {
		t.Fatalf("unexpected error rotating refresh token 2: %v", err)
	}
	if refToken3 == refToken2 {
		t.Errorf("expected new refresh token string after rotation 2")
	}
	if accToken3 == accToken2 {
		t.Errorf("expected new access token")
	}
}

func TestRefreshTokenGraceWindowRetry(t *testing.T) {
	ctx := context.Background()
	refreshRepo := newMemoryRefreshTokenRepo()
	userRepo := &memoryUserRepo{users: make(map[uuid.UUID]*domain.User)}

	user := &domain.User{
		ID:           uuid.New(),
		Phone:        "+919876543210",
		Role:         domain.RoleTenant,
		TokenVersion: 1,
	}
	_ = userRepo.Create(ctx, user)

	svc := NewService(nil, userRepo, nil, nil, nil, "otp-secret", "jwt-secret-very-secure-1234567890")
	svc.SetRefreshTokenRepo(refreshRepo)

	// Step 1: Issue Session
	_, refToken1, err := svc.IssueSession(ctx, user)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}

	// Step 2: First rotation (refToken1 -> refToken2)
	_, refToken2, _, err := svc.RotateRefreshToken(ctx, refToken1)
	if err != nil {
		t.Fatalf("first rotation: %v", err)
	}
	if refToken2 == "" {
		t.Fatalf("expected non-empty refToken2")
	}

	// Step 3: Network retransmit of refToken1 within 30s grace window
	accTokenRetry, refTokenRetry, _, err := svc.RotateRefreshToken(ctx, refToken1)
	if err != nil {
		t.Fatalf("expected grace window to tolerate retransmit, got error: %v", err)
	}
	if accTokenRetry == "" || refTokenRetry == "" {
		t.Fatalf("expected non-empty tokens on grace window retry")
	}
}

func TestRefreshTokenReplayAttackDetection(t *testing.T) {
	ctx := context.Background()
	refreshRepo := newMemoryRefreshTokenRepo()
	userRepo := &memoryUserRepo{users: make(map[uuid.UUID]*domain.User)}

	user := &domain.User{
		ID:           uuid.New(),
		Phone:        "+919876543210",
		Role:         domain.RoleTenant,
		TokenVersion: 1,
	}
	_ = userRepo.Create(ctx, user)

	svc := NewService(nil, userRepo, nil, nil, nil, "otp-secret", "jwt-secret-very-secure-1234567890")
	svc.SetRefreshTokenRepo(refreshRepo)

	// Step 1: Issue Session
	_, refToken1, err := svc.IssueSession(ctx, user)
	if err != nil {
		t.Fatalf("issue session: %v", err)
	}

	// Step 2: Legitimate rotation (refToken1 -> refToken2)
	_, refToken2, _, err := svc.RotateRefreshToken(ctx, refToken1)
	if err != nil {
		t.Fatalf("legitimate rotation: %v", err)
	}

	// Fast-forward revoked_at beyond 30s grace window to simulate delayed replay attack
	h1 := HashRefreshToken(refToken1)
	if rt1, ok := refreshRepo.tokens[h1]; ok {
		past := time.Now().Add(-35 * time.Second)
		rt1.RevokedAt = &past
	}

	// Step 3: Attacker replays already-used refToken1
	_, _, _, err = svc.RotateRefreshToken(ctx, refToken1)
	if err == nil || err != ErrReplayDetected {
		t.Fatalf("expected ErrReplayDetected, got %v", err)
	}

	// Step 4: Verification that entire family is invalidated
	// Even the legitimate refToken2 must now fail because family was revoked!
	_, _, _, err = svc.RotateRefreshToken(ctx, refToken2)
	if err == nil || err != ErrReplayDetected {
		t.Fatalf("expected ErrReplayDetected when using refToken2 from revoked family, got %v", err)
	}
}
