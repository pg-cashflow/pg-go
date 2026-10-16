# Ticket C.5: SMTP Mailer Fail-Closed Guard & Production Boot Check

- **Type**: `wayfinder:task`
- **Status**: Open
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Standard**: ASD-STE100
- **Builds on**: `docs/specs/auth-frontend-backend-contract.md`

---

## 1. Objective

Prevent silent step-up OTP delivery loss by detecting `mailer.NoopMailer` at runtime and enforcing mandatory `SMTP_HOST` configuration during production boot.

---

## 2. Technical Findings & Root Cause

1. In `internal/app/channels.go:18`, if `cfg.SMTPHost == ""`, the application initialises:
   ```go
   var mail mailer.Mailer = mailer.NoopMailer{}
   ```
2. In `internal/mailer/noop.go:8`, `NoopMailer.Send()` returns `nil` and sends no email.
3. In `internal/auth/service.go:180-195`, the code checks `s.mailer != nil`. Because `NoopMailer{}` is a non-nil interface value, `s.mailer != nil` evaluates to `true`.
4. As a result, when SMTP is unconfigured or misconfigured:
   - `RequestStepUpOTPWithChannel` returns `channel: "email"`, `err: nil`.
   - The API returns HTTP 200 OK.
   - No email is delivered.
   - The property owner never receives the payout approval code and cannot approve financial batches.
5. In `internal/config/config.go:162-173`, `AppEnv == "production"` validates `FirebaseProjectID` and payout secrets, but does not validate `SMTPHost`.

---

## 3. Implementation Steps

1. **Mailer Noop Type Detection:**
   - In `internal/mailer/noop.go`, add a type helper or check:
     ```go
     // IsNoop reports whether a mailer implementation is a NoopMailer.
     func IsNoop(m Mailer) bool {
         if m == nil {
             return true
         }
         _, ok := m.(NoopMailer)
         return ok
     }
     ```
2. **Fail-Closed Step-Up Enforcement:**
   - In `internal/auth/service.go` (`RequestStepUpOTPWithChannel`):
     ```go
     mailerAvailable := s.mailer != nil && !mailer.IsNoop(s.mailer)
     if purpose == "payout_approval" {
         if userEmail == "" || !mailerAvailable {
             return "", "", ErrNoDeliveryChannel
         }
         channel = "email"
         destination = userEmail
     }
     ```
3. **Production Boot Validation:**
   - In `internal/config/config.go`:
     ```go
     if cfg.AppEnv == "production" {
         if strings.TrimSpace(cfg.SMTPHost) == "" {
             return nil, fmt.Errorf("SMTP_HOST is required in production for payout step-up OTP delivery")
         }
     }
     ```

---

## 4. Karpathy Test Plan (One Example First)

### Target Test: `TestStepUpOTP_NoopMailer_FailsClosed`
```go
func TestStepUpOTP_NoopMailer_FailsClosed(t *testing.T) {
	ctx := context.Background()
	otpRepo := &mockOTPRepo{}
	secret := "test-secret-at-least-32-bytes-long"
	phone := "+919876543210"
	email := "owner@pgcashflow.com"
	batchID := uuid.New()

	users := &stubUsers{byPhone: map[string]*domain.User{
		phone: {ID: uuid.New(), Phone: phone, Email: email, Role: domain.RoleOwner},
	}}

	// NoopMailer passed as mailer implementation
	svc := NewService(otpRepo, users, &stubTenantsAuth{}, stubProps{}, sms.NoopGateway{}, secret, "jwt-secret")
	svc.SetMailer(mailer.NoopMailer{})

	// Must fail closed with ErrNoDeliveryChannel instead of silently succeeding
	_, _, err := svc.RequestStepUpOTPWithChannel(ctx, phone, "payout_approval", &batchID)
	if !errors.Is(err, ErrNoDeliveryChannel) {
		t.Fatalf("expected ErrNoDeliveryChannel when mailer is NoopMailer, got %v", err)
	}
}
```
