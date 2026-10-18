package auth

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

func TestService_ListSessionsAndRevoke(t *testing.T) {
	repo := newMemoryRefreshTokenRepo()
	svc := &Service{
		jwtSecret:   "test-secret-key-that-is-at-least-32-bytes-long",
		refreshRepo: repo,
	}

	ctx := context.Background()
	userID := uuid.New()
	user := &domain.User{
		ID:           userID,
		Role:         domain.RoleTenant,
		TokenVersion: 1,
	}

	// 1. Issue session 1 (iPhone Safari)
	ua1 := "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1"
	ip1 := "203.0.113.1"
	acc1, ref1, err := svc.IssueSessionWithClient(ctx, user, ua1, ip1)
	if err != nil {
		t.Fatalf("IssueSessionWithClient 1: %v", err)
	}

	claims1, err := VerifyToken(svc.jwtSecret, acc1)
	if err != nil {
		t.Fatalf("VerifyToken 1: %v", err)
	}
	if claims1.FamilyID == nil {
		t.Fatal("expected FamilyID in claims1")
	}

	// 2. Issue session 2 (Windows Chrome)
	ua2 := "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"
	ip2 := "198.51.100.2"
	_, _, err = svc.IssueSessionWithClient(ctx, user, ua2, ip2)
	if err != nil {
		t.Fatalf("IssueSessionWithClient 2: %v", err)
	}

	// 3. List active sessions from perspective of Session 1
	sessions, err := svc.ListSessions(ctx, userID, claims1.FamilyID, ref1)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected 2 active sessions, got %d", len(sessions))
	}

	var currentCount int
	var otherSessionID uuid.UUID
	for _, s := range sessions {
		if s.IsCurrent {
			currentCount++
			if s.ID != *claims1.FamilyID {
				t.Errorf("expected current session ID to match claims1 family ID %v, got %v", *claims1.FamilyID, s.ID)
			}
			if s.DeviceLabel != "Safari on iPhone" {
				t.Errorf("expected Safari on iPhone, got %s", s.DeviceLabel)
			}
		} else {
			otherSessionID = s.ID
			if s.DeviceLabel != "Chrome on Windows" {
				t.Errorf("expected Chrome on Windows, got %s", s.DeviceLabel)
			}
		}
	}
	if currentCount != 1 {
		t.Errorf("expected exactly 1 current session, got %d", currentCount)
	}

	// 4. Revoke the other session
	if err := svc.RevokeDeviceSession(ctx, userID, otherSessionID); err != nil {
		t.Fatalf("RevokeDeviceSession: %v", err)
	}

	// 5. List active sessions again
	sessionsAfter, err := svc.ListSessions(ctx, userID, claims1.FamilyID, ref1)
	if err != nil {
		t.Fatalf("ListSessions after revoke: %v", err)
	}
	if len(sessionsAfter) != 1 {
		t.Fatalf("expected 1 active session after revoke, got %d", len(sessionsAfter))
	}
	if sessionsAfter[0].ID != *claims1.FamilyID {
		t.Errorf("expected remaining session to be session 1, got %v", sessionsAfter[0].ID)
	}
}

func TestService_RevokeOtherSessions(t *testing.T) {
	repo := newMemoryRefreshTokenRepo()
	svc := &Service{
		jwtSecret:   "test-secret-key-that-is-at-least-32-bytes-long",
		refreshRepo: repo,
	}

	ctx := context.Background()
	userID := uuid.New()
	user := &domain.User{
		ID:           userID,
		Role:         domain.RoleTenant,
		TokenVersion: 1,
	}

	// 1. Issue current session (Session 1)
	acc1, ref1, err := svc.IssueSessionWithClient(ctx, user, "Safari on iPhone", "203.0.113.1")
	if err != nil {
		t.Fatalf("IssueSessionWithClient 1: %v", err)
	}
	claims1, err := VerifyToken(svc.jwtSecret, acc1)
	if err != nil || claims1.FamilyID == nil {
		t.Fatalf("VerifyToken 1: %v", err)
	}

	// 2. Issue other sessions (Session 2 and Session 3)
	_, _, err = svc.IssueSessionWithClient(ctx, user, "Chrome on Windows", "198.51.100.2")
	if err != nil {
		t.Fatalf("IssueSessionWithClient 2: %v", err)
	}
	_, _, err = svc.IssueSessionWithClient(ctx, user, "Firefox on Linux", "198.51.100.3")
	if err != nil {
		t.Fatalf("IssueSessionWithClient 3: %v", err)
	}

	// 3. Confirm 3 active sessions exist
	sessions, err := svc.ListSessions(ctx, userID, claims1.FamilyID, ref1)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 3 {
		t.Fatalf("expected 3 active sessions, got %d", len(sessions))
	}

	// 4. Revoke other sessions from the perspective of Session 1
	if err := svc.RevokeOtherSessions(ctx, userID, claims1.FamilyID, ref1); err != nil {
		t.Fatalf("RevokeOtherSessions: %v", err)
	}

	// 5. Assert only Session 1 remains active
	remaining, err := svc.ListSessions(ctx, userID, claims1.FamilyID, ref1)
	if err != nil {
		t.Fatalf("ListSessions after revoke others: %v", err)
	}
	if len(remaining) != 1 {
		t.Fatalf("expected exactly 1 remaining session, got %d", len(remaining))
	}
	if remaining[0].ID != *claims1.FamilyID {
		t.Errorf("expected remaining session to be current session %v, got %v", *claims1.FamilyID, remaining[0].ID)
	}
	if !remaining[0].IsCurrent {
		t.Errorf("expected remaining session to be marked as is_current")
	}

	// 6. Assert error when neither family ID nor refresh token is provided
	if err := svc.RevokeOtherSessions(ctx, userID, nil, ""); err != ErrNoCurrentSession {
		t.Errorf("expected ErrNoCurrentSession, got %v", err)
	}
}
