package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pg-cashflow/pg-go/internal/domain"
	"github.com/pg-cashflow/pg-go/internal/mailer"
	"github.com/pg-cashflow/pg-go/internal/postgres"
	"github.com/pg-cashflow/pg-go/internal/sms"
)

type mockOTPRepo struct {
	mu       sync.Mutex
	requests []*postgres.OTPRequest
}

func (m *mockOTPRepo) Create(_ context.Context, req *postgres.OTPRequest) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	req.ID = uuid.New()
	if req.CreatedAt.IsZero() {
		req.CreatedAt = time.Now().UTC()
	}
	m.requests = append(m.requests, req)
	return nil
}

func (m *mockOTPRepo) CreateWithQuota(_ context.Context, req *postgres.OTPRequest, window time.Duration, max int, cooldown time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now().UTC()
	if cooldown > 0 {
		cooldownSince := now.Add(-cooldown)
		for _, r := range m.requests {
			if r.Phone == req.Phone && (req.Purpose == "" || r.Purpose == req.Purpose) && !r.CreatedAt.Before(cooldownSince) {
				rem := cooldown - now.Sub(r.CreatedAt)
				if rem < time.Second {
					rem = time.Second
				}
				return &RateLimitError{Message: ErrRateLimited.Error(), RetryAfter: rem}
			}
		}
	}

	since := now.Add(-window)
	count := 0
	var oldest time.Time
	for _, r := range m.requests {
		if r.Phone == req.Phone && (req.Purpose == "" || r.Purpose == req.Purpose) && !r.CreatedAt.Before(since) {
			if count == 0 {
				oldest = r.CreatedAt
			}
			count++
		}
	}
	if count >= max {
		rem := window - now.Sub(oldest)
		if rem < time.Second {
			rem = time.Second
		}
		return &RateLimitError{Message: ErrRateLimited.Error(), RetryAfter: rem}
	}

	req.ID = uuid.New()
	if req.CreatedAt.IsZero() {
		req.CreatedAt = now
	}
	m.requests = append(m.requests, req)
	return nil
}

func (m *mockOTPRepo) LatestUnused(_ context.Context, phone string) (*postgres.OTPRequest, error) {
	return m.LatestUnusedByPurpose(context.Background(), phone, "", nil)
}

func (m *mockOTPRepo) LatestUnusedByPurpose(_ context.Context, phone string, purpose string, batchID *uuid.UUID) (*postgres.OTPRequest, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.requests) - 1; i >= 0; i-- {
		r := m.requests[i]
		if r.Phone == phone && !r.Used {
			if purpose != "" && r.Purpose != "" && r.Purpose != purpose {
				continue
			}
			if batchID != nil {
				if r.BatchID == nil || *r.BatchID != *batchID {
					continue
				}
			}
			return r, nil
		}
	}
	return nil, pgx.ErrNoRows
}

func (m *mockOTPRepo) IncrementAttempts(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.requests {
		if r.ID == id {
			if r.Attempts >= 5 {
				return postgres.ErrOTPLocked
			}
			r.Attempts++
			return nil
		}
	}
	return nil
}

func (m *mockOTPRepo) MarkUsed(_ context.Context, id uuid.UUID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.requests {
		if r.ID == id {
			if r.Used {
				return domain.ErrOTPAlreadyUsed
			}
			r.Used = true
			return nil
		}
	}
	return domain.ErrOTPAlreadyUsed
}

func (m *mockOTPRepo) CountRecent(_ context.Context, phone string, since time.Time) (int, error) {
	return m.CountRecentByPurpose(context.Background(), phone, "", since)
}

func (m *mockOTPRepo) CountRecentByPurpose(_ context.Context, phone string, purpose string, since time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	count := 0
	for _, r := range m.requests {
		if r.Phone == phone && (purpose == "" || r.Purpose == purpose) && !r.CreatedAt.Before(since) {
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
		name     string
		authTime time.Time
		maxAge   time.Duration
		wantErr  error
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

func TestVerifyStepUpOTP_PurposeAndBatchBinding(t *testing.T) {
	ctx := context.Background()
	otpRepo := &mockOTPRepo{}
	smsGateway := &mockSMSGateway{}
	secret := "test-secret-at-least-32-bytes-long"

	svc := NewService(otpRepo, nil, nil, nil, smsGateway, secret, "jwt-secret")
	phone := "+919876543210"
	batchA := uuid.New()
	batchB := uuid.New()

	// 1. Request login OTP
	if err := svc.RequestOTPWithPurpose(ctx, phone, "login"); err != nil {
		t.Fatalf("request login OTP: %v", err)
	}
	loginMsg := smsGateway.sentMessages[len(smsGateway.sentMessages)-1]
	loginCode := strings.TrimSuffix(strings.Split(loginMsg, " ")[4], ".")

	// Login OTP must NOT be valid for payout approval step-up
	if err := svc.VerifyStepUpOTPSpecific(ctx, phone, loginCode, "payout_approval", &batchA); err != ErrInvalidOTP {
		t.Fatalf("expected ErrInvalidOTP when using login OTP for payout approval, got: %v", err)
	}

	// 2. Request payout approval OTP for batch A
	if err := svc.RequestStepUpOTP(ctx, phone, "payout_approval", &batchA); err != nil {
		t.Fatalf("request payout approval OTP for batch A: %v", err)
	}
	batchAMsg := smsGateway.sentMessages[len(smsGateway.sentMessages)-1]
	batchACode := strings.TrimSuffix(strings.Split(batchAMsg, " ")[5], ".")

	// OTP for batch A must NOT be valid for batch B
	if err := svc.VerifyStepUpOTPSpecific(ctx, phone, batchACode, "payout_approval", &batchB); err != ErrInvalidOTP {
		t.Fatalf("expected ErrInvalidOTP when using batch A OTP for batch B, got: %v", err)
	}

	// OTP for batch A MUST succeed for batch A
	if err := svc.VerifyStepUpOTPSpecific(ctx, phone, batchACode, "payout_approval", &batchA); err != nil {
		t.Fatalf("expected success when verifying batch A OTP for batch A, got: %v", err)
	}
}

type raceMockOTPRepo struct {
	mockOTPRepo
	readBarrier sync.WaitGroup
	startMark   chan struct{}
}

func (r *raceMockOTPRepo) LatestUnusedByPurpose(ctx context.Context, phone string, purpose string, batchID *uuid.UUID) (*postgres.OTPRequest, error) {
	req, err := r.mockOTPRepo.LatestUnusedByPurpose(ctx, phone, purpose, batchID)
	if err != nil {
		return nil, err
	}
	copied := *req
	r.readBarrier.Done()
	<-r.startMark
	return &copied, nil
}

func TestVerifyStepUpOTP_ConcurrentRaceExactlyOneWins(t *testing.T) {
	ctx := context.Background()
	smsGateway := &mockSMSGateway{}
	const concurrency = 10

	otpRepo := &raceMockOTPRepo{
		startMark: make(chan struct{}),
	}
	otpRepo.readBarrier.Add(concurrency)

	svc := NewService(otpRepo, nil, nil, nil, smsGateway, "otp-secret", "jwt-secret")

	phone := "+919876543210"
	batchID := uuid.New()

	if err := svc.RequestStepUpOTP(ctx, phone, "payout_approval", &batchID); err != nil {
		t.Fatalf("request step up OTP: %v", err)
	}
	lastMsg := smsGateway.sentMessages[len(smsGateway.sentMessages)-1]
	code := strings.TrimSuffix(strings.Split(lastMsg, " ")[5], ".")

	var wg sync.WaitGroup
	var mu sync.Mutex
	successCount := 0
	invalidCount := 0
	otherErrors := 0

	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go func() {
			defer wg.Done()
			err := svc.VerifyStepUpOTPSpecific(ctx, phone, code, "payout_approval", &batchID)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				successCount++
			case errors.Is(err, ErrInvalidOTP), errors.Is(err, ErrOTPLocked):
				invalidCount++
			default:
				otherErrors++
			}
		}()
	}

	// Wait until all workers have read the unconsumed OTP (reproducing TOCTOU condition)
	otpRepo.readBarrier.Wait()
	// Release all workers simultaneously to race on MarkUsed
	close(otpRepo.startMark)

	wg.Wait()

	if successCount != 1 {
		t.Errorf("expected exactly 1 successful OTP verification, got %d", successCount)
	}
	if invalidCount != concurrency-1 {
		t.Errorf("expected %d invalid OTP errors, got %d", concurrency-1, invalidCount)
	}
	if otherErrors != 0 {
		t.Errorf("expected 0 other errors, got %d", otherErrors)
	}
}

func TestRequestOTP_SeparatedRateLimitsByPurpose(t *testing.T) {
	ctx := context.Background()
	otpRepo := &mockOTPRepo{}
	smsGateway := &mockSMSGateway{}
	secret := "test-secret-at-least-32-bytes-long"

	svc := NewService(otpRepo, nil, nil, nil, smsGateway, secret, "jwt-secret")
	svc.SetCooldownWindow(0)
	phone := "+919876543210"

	// Exhaust 3 login OTPs
	for i := 0; i < 3; i++ {
		if err := svc.RequestOTPWithPurpose(ctx, phone, "login"); err != nil {
			t.Fatalf("expected login OTP request %d to succeed, got %v", i+1, err)
		}
	}

	// 4th login OTP must be rate limited
	if err := svc.RequestOTPWithPurpose(ctx, phone, "login"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited for 4th login OTP, got %v", err)
	}

	// Payout approval OTP must NOT be blocked by exhausted login limit
	if err := svc.RequestOTPWithPurpose(ctx, phone, "payout_approval"); err != nil {
		t.Fatalf("expected payout approval OTP to succeed despite login limit, got %v", err)
	}
}

func TestRequestOTP_NoAccountAntiEnumeration(t *testing.T) {
	ctx := context.Background()
	otpRepo := &mockOTPRepo{}
	smsGateway := &mockSMSGateway{}
	secret := "test-secret-at-least-32-bytes-long"

	registeredPhone := "+919876543210"
	unknownPhone := "+919111111111"

	users := &stubUsers{byPhone: map[string]*domain.User{
		registeredPhone: {ID: uuid.New(), Phone: registeredPhone},
	}}

	svc := NewService(otpRepo, users, &stubTenantsAuth{}, stubProps{}, smsGateway, secret, "jwt-secret")

	// Unknown phone must return nil (anti-enumeration) but NOT send any SMS
	if err := svc.RequestOTP(ctx, unknownPhone); err != nil {
		t.Fatalf("expected nil error for unknown phone anti-enumeration, got %v", err)
	}
	if len(smsGateway.sentMessages) != 0 {
		t.Fatalf("expected 0 SMS sent for unknown phone, got %d", len(smsGateway.sentMessages))
	}

	// Registered phone must send SMS
	if err := svc.RequestOTP(ctx, registeredPhone); err != nil {
		t.Fatalf("expected nil error for registered phone, got %v", err)
	}
	if len(smsGateway.sentMessages) != 1 {
		t.Fatalf("expected 1 SMS sent for registered phone, got %d", len(smsGateway.sentMessages))
	}
}

type mockMailer struct {
	sentTo   []string
	subjects []string
	bodies   []string
}

func (m *mockMailer) Send(_ context.Context, to, subject, htmlBody string) error {
	m.sentTo = append(m.sentTo, to)
	m.subjects = append(m.subjects, subject)
	m.bodies = append(m.bodies, htmlBody)
	return nil
}

func TestMaskEmail(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"john.doe@example.com", "j***e@example.com"},
		{"a@b.com", "a***@b.com"},
		{"ab@example.com", "a***@example.com"},
		{"abc@example.com", "a***c@example.com"},
		{"invalid", "***"},
		{"", "***"},
		{"@domain.com", "***"},
		{"user@", "***"},
	}
	for _, tc := range cases {
		got := MaskEmail(tc.input)
		if got != tc.want {
			t.Errorf("MaskEmail(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestRequestStepUpOTP_DualChannelDeliveryAndMasking(t *testing.T) {
	ctx := context.Background()
	otpRepo := &mockOTPRepo{}
	secret := "test-secret-at-least-32-bytes-long"
	phone := "+919876543210"
	email := "owner@pgcashflow.com"
	batchID := uuid.New()

	users := &stubUsers{byPhone: map[string]*domain.User{
		phone: {ID: uuid.New(), Phone: phone, Email: email, Role: domain.RoleOwner},
	}}

	// 1. With NoopGateway and valid mailer -> delivers via Email
	mm := &mockMailer{}
	svc := NewService(otpRepo, users, &stubTenantsAuth{}, stubProps{}, sms.NoopGateway{}, secret, "jwt-secret")
	svc.SetMailer(mm)

	ch, dest, err := svc.RequestStepUpOTPWithChannel(ctx, phone, "payout_approval", &batchID)
	if err != nil {
		t.Fatalf("expected email delivery to succeed, got %v", err)
	}
	if ch != "email" {
		t.Errorf("expected channel 'email', got %q", ch)
	}
	if dest != "o***r@pgcashflow.com" {
		t.Errorf("expected masked email 'o***r@pgcashflow.com', got %q", dest)
	}
	if len(mm.sentTo) != 1 || mm.sentTo[0] != email {
		t.Fatalf("expected mailer to send to %q, got %v", email, mm.sentTo)
	}
	if !strings.Contains(mm.subjects[0], "Payout Approval") {
		t.Errorf("expected subject to contain 'Payout Approval', got %q", mm.subjects[0])
	}

	// 2. Rate limiting check: rate limits stay bound to canonical phone identity
	svc.SetCooldownWindow(0)
	_ = svc.RequestStepUpOTP(ctx, phone, "payout_approval", &batchID)
	_ = svc.RequestStepUpOTP(ctx, phone, "payout_approval", &batchID)

	// 4th request must fail with ErrRateLimited
	_, _, err = svc.RequestStepUpOTPWithChannel(ctx, phone, "payout_approval", &batchID)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited on 4th attempt, got %v", err)
	}

	// 3. Without mailer and with NoopGateway -> fails closed with ErrNoDeliveryChannel
	freshPhone := "+919888877777"
	freshUsers := &stubUsers{byPhone: map[string]*domain.User{
		freshPhone: {ID: uuid.New(), Phone: freshPhone, Email: "fresh@example.com"},
	}}
	svcNoMailer := NewService(otpRepo, freshUsers, &stubTenantsAuth{}, stubProps{}, sms.NoopGateway{}, secret, "jwt-secret")
	_, _, err = svcNoMailer.RequestStepUpOTPWithChannel(ctx, freshPhone, "payout_approval", &batchID)
	if !errors.Is(err, ErrNoDeliveryChannel) {
		t.Fatalf("expected ErrNoDeliveryChannel when neither channel is available, got %v", err)
	}
}

func TestRequestStepUpOTP_CooldownEnforced(t *testing.T) {
	ctx := context.Background()
	otpRepo := &mockOTPRepo{}
	secret := "test-secret-at-least-32-bytes-long"
	phone := "+919876543210"
	email := "owner@pgcashflow.com"
	batchID := uuid.New()

	users := &stubUsers{byPhone: map[string]*domain.User{
		phone: {ID: uuid.New(), Phone: phone, Email: email, Role: domain.RoleOwner},
	}}

	mm := &mockMailer{}
	svc := NewService(otpRepo, users, &stubTenantsAuth{}, stubProps{}, sms.NoopGateway{}, secret, "jwt-secret")
	svc.SetMailer(mm)

	// First request succeeds
	_, _, err := svc.RequestStepUpOTPWithChannel(ctx, phone, "payout_approval", &batchID)
	if err != nil {
		t.Fatalf("expected first OTP request to succeed, got %v", err)
	}

	// Immediate second request within 30s fails with ErrRateLimited
	_, _, err = svc.RequestStepUpOTPWithChannel(ctx, phone, "payout_approval", &batchID)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited due to cooldown, got %v", err)
	}
}

// TestRequestStepUpOTP_NoopMailerFailsClosed is the ticket C.5 regression
// test. It deliberately reproduces the real production wiring
// (internal/app/bootstrap.go always calls authSvc.SetMailer(mail), and
// internal/app/channels.go hands it mailer.NoopMailer{} whenever SMTP_HOST
// is unset) rather than the pre-fix test fixture state, which only ever
// left s.mailer as Go nil (never calling SetMailer at all). A nil mailer
// was already caught by the old "s.mailer != nil" check; an explicitly-set
// NoopMailer was not â€” that gap is what let delivery fail silently while
// the API reported 200 OK.
//
// It also covers a non-payout_approval purpose, because the original
// proposed patch gated the fix on `purpose == "payout_approval"` only and
// would have left every other step-up purpose (see internal/api/handlers_step_up.go,
// which reads an arbitrary purpose from caller input) with the identical bug.
func TestRequestStepUpOTP_NoopMailerFailsClosed(t *testing.T) {
	ctx := context.Background()
	secret := "test-secret-at-least-32-bytes-long"
	phone := "+919876543210"
	email := "owner@pgcashflow.com"
	batchID := uuid.New()

	users := &stubUsers{byPhone: map[string]*domain.User{
		phone: {ID: uuid.New(), Phone: phone, Email: email, Role: domain.RoleOwner},
	}}

	t.Run("payout_approval purpose fails closed, not 200-with-nothing-sent", func(t *testing.T) {
		otpRepo := &mockOTPRepo{}
		svc := NewService(otpRepo, users, &stubTenantsAuth{}, stubProps{}, sms.NoopGateway{}, secret, "jwt-secret")
		svc.SetMailer(mailer.NoopMailer{})

		_, _, err := svc.RequestStepUpOTPWithChannel(ctx, phone, "payout_approval", &batchID)
		if !errors.Is(err, ErrNoDeliveryChannel) {
			t.Fatalf("expected ErrNoDeliveryChannel with NoopMailer, got %v (nil means the API would have returned 200 while sending nothing)", err)
		}
	})

	t.Run("non-payout purpose also fails closed (fix must not be purpose-specific)", func(t *testing.T) {
		otpRepo := &mockOTPRepo{}
		svc := NewService(otpRepo, users, &stubTenantsAuth{}, stubProps{}, sms.NoopGateway{}, secret, "jwt-secret")
		svc.SetMailer(mailer.NoopMailer{})

		_, _, err := svc.RequestStepUpOTPWithChannel(ctx, phone, "sensitive_profile_change", &batchID)
		if !errors.Is(err, ErrNoDeliveryChannel) {
			t.Fatalf("expected ErrNoDeliveryChannel for non-payout purpose with NoopMailer, got %v", err)
		}
	})

	t.Run("real mailer still delivers (no false-positive fail-closed)", func(t *testing.T) {
		otpRepo := &mockOTPRepo{}
		mm := &mockMailer{}
		svc := NewService(otpRepo, users, &stubTenantsAuth{}, stubProps{}, sms.NoopGateway{}, secret, "jwt-secret")
		svc.SetMailer(mm)

		ch, _, err := svc.RequestStepUpOTPWithChannel(ctx, phone, "payout_approval", &batchID)
		if err != nil {
			t.Fatalf("expected success with a real mailer, got %v", err)
		}
		if ch != "email" || len(mm.sentTo) != 1 {
			t.Fatalf("expected one real email send, got channel=%q sent=%v", ch, mm.sentTo)
		}
	})

	t.Run("atomic attempt quota locks after 5 attempts", func(t *testing.T) {
		otpRepo := &mockOTPRepo{}
		svc := NewService(otpRepo, users, &stubTenantsAuth{}, stubProps{}, sms.NoopGateway{}, secret, "jwt-secret")
		reqID := uuid.New()
		validOTP := "123456"
		otpHash := HashOTP(secret, validOTP)

		otpRepo.requests = append(otpRepo.requests, &postgres.OTPRequest{
			ID:          reqID,
			Phone:       phone,
			OTPHash:     otpHash,
			Attempts:    0,
			ExpiresAt:   time.Now().UTC().Add(5 * time.Minute),
			Used:        false,
			Purpose:     "payout_approval",
			BatchID:     &batchID,
			Channel:     "sms",
			Destination: phone,
			CreatedAt:   time.Now().UTC(),
		})

		// Make 5 wrong attempts
		for i := 0; i < 5; i++ {
			err := svc.VerifyStepUpOTPSpecific(ctx, phone, "000000", "payout_approval", &batchID)
			if !errors.Is(err, ErrInvalidOTP) && !errors.Is(err, ErrOTPLocked) {
				t.Fatalf("attempt %d: expected ErrInvalidOTP or ErrOTPLocked, got %v", i+1, err)
			}
		}

		// 6th attempt with the CORRECT OTP must be blocked with ErrOTPLocked
		err := svc.VerifyStepUpOTPSpecific(ctx, phone, validOTP, "payout_approval", &batchID)
		if !errors.Is(err, ErrOTPLocked) {
			t.Fatalf("expected ErrOTPLocked on 6th attempt with valid OTP, got %v", err)
		}
	})
}

