# Ticket C.4: Deprecate and Disable Legacy Phone OTP Login Routes

- **Type**: `wayfinder:task`
- **Status**: Open
- **Parent**: [Wayfinder Map](file:///c:/Users/divak/Downloads/pg-go/docs/wayfinder/map.md)
- **Standard**: ASD-STE100
- **Builds on**: `docs/specs/auth-frontend-backend-contract.md`

---

## 1. Objective

Disable the public unauthenticated endpoints `POST /api/auth/otp/request` and `POST /api/auth/otp/verify` to eliminate an insecure entry point that circumvents Firebase Phone Auth device attestation and silently downgrades SMS delivery to email.

---

## 2. Technical Findings & Root Cause

1. In `internal/api/router_api.go:20-21`, the routes are currently registered:
   ```go
   api.POST("/auth/otp/request", ipRateLimit(3.0/60, 5), h.OTPRequest)
   api.POST("/auth/otp/verify", ipRateLimit(10.0/60, 20), h.OTPVerify)
   ```
2. Because `internal/app/channels.go` initializes `sms.NoopGateway{}`, `smsAvailable` is false in production.
3. When an unauthenticated caller POSTs to `/api/auth/otp/request`, `RequestStepUpOTPWithChannel` falls through to email. It delivers a phone login verification code to the user's email address.
4. This creates a redundant login path that circumvents Firebase CAPTCHA and SafetyNet/Play Integrity checks.

---

## 3. Implementation Steps

1. **Handler Deprecation Response:**
   - In `internal/api/handlers_public.go`:
   - Modify `h.OTPRequest` and `h.OTPVerify` to return `410 Gone`:
     ```json
     {
       "error": "custom phone OTP login is disabled; use Firebase authentication",
       "code": "AUTH_ENDPOINT_DISABLED"
     }
     ```
2. **Contract & Test Synchronization:**
   - Update `internal/api/routes_outcome_test.go`:
     - Assert that `POST /api/auth/otp/request` returns HTTP 410 Gone.
     - Assert that `POST /api/auth/otp/verify` returns HTTP 410 Gone.
   - Update `internal/api/handlers_auth_http_test.go` to assert 410 responses.

---

## 4. Karpathy Test Plan (One Example First)

### Target Test: `TestOTPRequest_Disabled_Returns410`
```go
func TestOTPRequest_Disabled_Returns410(t *testing.T) {
	router := gin.New()
	h := &Handlers{}
	router.POST("/auth/otp/request", h.OTPRequest)

	body := bytes.NewBufferString(`{"phone":"+919876543210"}`)
	req := httptest.NewRequest(http.MethodPost, "/auth/otp/request", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	router.ServeHTTP(w, req)

	if w.Code != http.StatusGone {
		t.Fatalf("expected HTTP 410 Gone, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "AUTH_ENDPOINT_DISABLED") {
		t.Fatalf("expected AUTH_ENDPOINT_DISABLED in body, got %s", w.Body.String())
	}
}
```
