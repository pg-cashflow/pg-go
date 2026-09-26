package auth

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/postgres"
)

type mockOTPRepo struct {
	requests []*postgres.OTPRequest
}

func (m *mockOTPRepo) Create(_ context.Context, req *postgres.OTPRequest) error {
	req.ID = uuid.New()
	m.requests = append(m.requests, req)
	return nil
}

func (m *mockOTPRepo) LatestUnused(_ context.Context, phone string) (*postgres.OTPRequest, error) {
	for i := len(m.requests) - 1; i >= 0; i-- {
		r := m.requests[i]
		if r.Phone == phone && !r.Used {
			return r, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (m *mockOTPRepo) IncrementAttempts(_ context.Context, id uuid.UUID) error {
	for _, r := range m.requests {
		if r.ID == id {
			r.Attempts++
			return nil
		}
	}
	return nil
}

func (m *mockOTPRepo) MarkUsed(_ context.Context, id uuid.UUID) error {
	for _, r := range m.requests {
		if r.ID == id {
			r.Used = true
			return nil
		}
	}
	return nil
}

func (m *mockOTPRepo) CountRecent(_ context.Context, phone string, since time.Time) (int, error) {
	count := 0
	for _, r := range m.requests {
		if r.Phone == phone && r.ExpiresAt.After(since) {
			count++
		}
	}
	return count, nil
}

type mockSMSGateway struct {
	sentMessages []string
	sentPhones   []string
}

func (g *mockSMSGateway) Send(_ context.Context, phone, message string) error {
	g.sentPhones = append(g.sentPhones, phone)
	g.sentMessages = append(g.sentMessages, message)
	return nil
}

func TestRequestOTPWithPurpose_CopyAndRateLimit(t *testing.T) {
	ctx := context.Background()
	otpRepo := &mockOTPRepo{}
	smsGateway := &mockSMSGateway{}
	secret := "test-secret-at-least-32-bytes-long"

	svc := NewService(otpRepo, nil, nil, nil, smsGateway, secret, "jwt-secret")

	// 1. Payout approval purpose
	phone := "+919876543210"
	if err := svc.RequestOTPWithPurpose(ctx, phone, "payout_approval"); err != nil {
		t.Fatalf("unexpected error requesting payout approval OTP: %v", err)
	}
	if len(smsGateway.sentMessages) != 1 {
		t.Fatalf("expected 1 message sent, got %d", len(smsGateway.sentMessages))
	}
	if !strings.Contains(smsGateway.sentMessages[0], "Your payout approval code is") {
		t.Errorf("expected payout approval message copy, got: %s", smsGateway.sentMessages[0])
	}

	// 2. Default / Login purpose
	if err := svc.RequestOTPWithPurpose(ctx, phone, "login"); err != nil {
		t.Fatalf("unexpected error requesting login OTP: %v", err)
	}
	if len(smsGateway.sentMessages) != 2 {
		t.Fatalf("expected 2 messages sent, got %d", len(smsGateway.sentMessages))
	}
	if !strings.Contains(smsGateway.sentMessages[1], "Your login OTP is") {
		t.Errorf("expected login OTP copy, got: %s", smsGateway.sentMessages[1])
	}
}

func TestVerifyStepUpOTP_ReplayAndLock(t *testing.T) {
	ctx := context.Background()
	otpRepo := &mockOTPRepo{}
	smsGateway := &mockSMSGateway{}
	secret := "test-secret-at-least-32-bytes-long"

	svc := NewService(otpRepo, nil, nil, nil, smsGateway, secret, "jwt-secret")
	phone := "+919876543210"

	// Request approval OTP
	if err := svc.RequestOTPWithPurpose(ctx, phone, "payout_approval"); err != nil {
		t.Fatalf("request approval OTP: %v", err)
	}
	if _, err := otpRepo.LatestUnused(ctx, phone); err != nil {
		t.Fatalf("load latest unused: %v", err)
	}

	// Extract generated code from SMS message
	msg := smsGateway.sentMessages[len(smsGateway.sentMessages)-1]
	parts := strings.Split(msg, " ")
	// "Your payout approval code is XXXXXX. Valid for 5 minutes."
	code := strings.TrimSuffix(parts[5], ".")

	// 1. Invalid OTP code
	if err := svc.VerifyStepUpOTP(ctx, phone, "000000"); err != ErrInvalidOTP {
		t.Errorf("expected ErrInvalidOTP on wrong code, got: %v", err)
	}

	// 2. Valid OTP code - must succeed
	if err := svc.VerifyStepUpOTP(ctx, phone, code); err != nil {
		t.Fatalf("expected success on valid code, got: %v", err)
	}

	// 3. Replay attack with same OTP code - must be blocked (already marked used)
	if err := svc.VerifyStepUpOTP(ctx, phone, code); err != ErrInvalidOTP {
		t.Errorf("expected ErrInvalidOTP on replayed OTP, got: %v", err)
	}

	// 4. Expired OTP
	expiredReq := &postgres.OTPRequest{
		Phone:     phone,
		OTPHash:   HashOTP(secret, "123456"),
		Attempts:  0,
		ExpiresAt: time.Now().UTC().Add(-time.Minute),
		Used:      false,
	}
	_ = otpRepo.Create(ctx, expiredReq)
	if err := svc.VerifyStepUpOTP(ctx, phone, "123456"); err != ErrOTPExpired {
		t.Errorf("expected ErrOTPExpired, got: %v", err)
	}

	// 5. Locked OTP (max attempts reached)
	lockedReq := &postgres.OTPRequest{
		Phone:     phone,
		OTPHash:   HashOTP(secret, "654321"),
		Attempts:  OTPMaxAttempts,
		ExpiresAt: time.Now().UTC().Add(time.Minute),
		Used:      false,
	}
	_ = otpRepo.Create(ctx, lockedReq)
	if err := svc.VerifyStepUpOTP(ctx, phone, "654321"); err != ErrOTPLocked {
		t.Errorf("expected ErrOTPLocked, got: %v", err)
	}
}

func TestVerifyFirebaseStepUp_Freshness(t *testing.T) {
	ctx := context.Background()
	secret := "test-secret-at-least-32-bytes-long"

	tests := []struct {
		name      string
		authTime  time.Time
		maxAge    time.Duration
		wantErr   error
	}{
		{
			name:     "fresh token (1 minute old)",
			authTime: time.Now().UTC().Add(-time.Minute),
			maxAge:   5 * time.Minute,
			wantErr:  nil,
		},
		{
			name:     "stale token (10 minutes old)",
			authTime: time.Now().UTC().Add(-10 * time.Minute),
			maxAge:   5 * time.Minute,
			wantErr:  ErrStaleAuthToken,
		},
		{
			name:     "zero auth_time fails safe",
			authTime: time.Time{},
			maxAge:   5 * time.Minute,
			wantErr:  ErrStaleAuthToken,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			verifier := stubFirebaseVerifier{
				uid:           "test-uid",
				phone:         "+919876543210",
				email:         "owner@test.com",
				emailVerified: true,
				authTime:      tc.authTime,
			}
			svc := NewService(nil, nil, nil, nil, nil, secret, "jwt-secret")
			svc.SetFirebaseVerifier(verifier)

			ident, err := svc.VerifyFirebaseStepUp(ctx, "mock-id-token", tc.maxAge)
			if tc.wantErr != nil {
				if err != tc.wantErr {
					t.Fatalf("expected error %v, got %v", tc.wantErr, err)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if ident.UID != "test-uid" {
					t.Errorf("expected UID test-uid, got %s", ident.UID)
				}
			}
		})
	}
}
