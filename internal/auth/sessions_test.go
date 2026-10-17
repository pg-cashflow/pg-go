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
