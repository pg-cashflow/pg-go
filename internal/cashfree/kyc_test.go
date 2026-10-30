package cashfree

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient wires a Client against the given test server URL and returns
// a client that hits the same URL for both payments and verification endpoints.
func newKYCTestClient(server *httptest.Server) *Client {
	return &Client{
		cfg:          Config{AppID: "test-id", SecretKey: "test-secret", Env: "sandbox"},
		http:         server.Client(),
		baseOverride: server.URL,
	}
}

func TestCashfreeKYCClient_NotConfigured(t *testing.T) {
	c := &Client{cfg: Config{}, http: http.DefaultClient}
	if _, err := c.RequestAadhaarOTP(context.Background(), "123412341234"); err == nil {
		t.Error("expected error when not configured")
	}
	if _, err := c.VerifyAadhaarOTP(context.Background(), "ref-1", "123456"); err == nil {
		t.Error("expected error when not configured")
	}
}

func TestAPIError_IsPermanent(t *testing.T) {
	cases := []struct {
		code      int
		permanent bool
	}{
		{400, true},
		{401, false}, // backend credentials invalid - transient from tenant standpoint
		{403, false}, // backend credentials forbidden - transient from tenant standpoint
		{404, true},
		{410, true},
		{422, true},
		{429, false}, // rate limit is transient
		{500, false}, // server error is transient
		{502, false},
		{503, false},
		{504, false},
	}
	for _, tc := range cases {
		err := &APIError{StatusCode: tc.code}
		if err.IsPermanent() != tc.permanent {
			t.Errorf("StatusCode %d: expected IsPermanent=%v, got %v", tc.code, tc.permanent, err.IsPermanent())
		}
	}
}

func TestAPIError_OmitBodyFromErrorString(t *testing.T) {
	err := &APIError{
		StatusCode: 404,
		Status:     "404 Not Found",
		Body:       `{"sensitive_pii": "Aadhaar number 1234 5678 9012"}`,
	}
	s := err.Error()
	if strings.Contains(s, "1234") || strings.Contains(s, "sensitive_pii") {
		t.Fatalf("APIError.Error() leaked response body: %s", s)
	}
	if !strings.Contains(s, "404") {
		t.Fatalf("expected status code in Error(): %s", s)
	}
}


func TestClassifyAadhaarError(t *testing.T) {
	tests := []struct {
		name         string
		httpStatus   int
		msg          string
		expectedCode string
		expectedCls  AadhaarFailureClass
	}{
		{
			name:         "Mobile not linked",
			httpStatus:   200,
			msg:          "Aadhaar number does not have mobile number",
			expectedCode: "MOBILE_NOT_LINKED",
			expectedCls:  FailureTerminal,
		},
		{
			name:         "Mobile not linked alternate phrasing",
			httpStatus:   200,
			msg:          "Mobile number not registered with Aadhaar",
			expectedCode: "MOBILE_NOT_LINKED",
			expectedCls:  FailureTerminal,
		},
		{
			name:         "Invalid Aadhaar card (first attempt recoverable)",
			httpStatus:   200,
			msg:          "Invalid Aadhaar Card",
			expectedCode: "INVALID_AADHAAR",
			expectedCls:  FailureRecoverable,
		},
		{
			name:         "Wrong OTP entered",
			httpStatus:   400,
			msg:          "OTP entered is invalid",
			expectedCode: "WRONG_OTP",
			expectedCls:  FailureRecoverable,
		},
		{
			name:         "OTP expired",
			httpStatus:   400,
			msg:          "Otp expired",
			expectedCode: "OTP_EXPIRED",
			expectedCls:  FailureRecoverable,
		},
		{
			name:         "Session expired",
			httpStatus:   400,
			msg:          "Session expired, please generate a new OTP",
			expectedCode: "SESSION_EXPIRED",
			expectedCls:  FailureRecoverable,
		},
		{
			name:         "Cooldown active throttle",
			httpStatus:   409,
			msg:          "Otp generated for this aadhaar, please try after some time",
			expectedCode: "COOLDOWN_ACTIVE",
			expectedCls:  FailureCooldown,
		},
		{
			name:         "Insufficient wallet balance (system fault)",
			httpStatus:   422,
			msg:          "Insufficient balance in Cashfree account",
			expectedCode: "PROVIDER_BALANCE",
			expectedCls:  FailureSystemFault,
		},
		{
			name:         "Cashfree 500 error (system fault)",
			httpStatus:   500,
			msg:          "something went wrong on UIDAI server",
			expectedCode: "PROVIDER_ERROR",
			expectedCls:  FailureSystemFault,
		},
		{
			name:         "Cashfree 503 error with cooldown message is system fault",
			httpStatus:   503,
			msg:          "Otp generated for this aadhaar, please try after some time",
			expectedCode: "PROVIDER_ERROR",
			expectedCls:  FailureSystemFault,
		},
		{
			name:         "Cashfree 403 IP not whitelisted (system fault)",
			httpStatus:   403,
			msg:          "IP address not whitelisted",
			expectedCode: "PROVIDER_AUTH_ERROR",
			expectedCls:  FailureSystemFault,
		},
		{
			name:         "Cashfree 401 Unauthorized (system fault)",
			httpStatus:   401,
			msg:          "Unauthorized request",
			expectedCode: "PROVIDER_AUTH_ERROR",
			expectedCls:  FailureSystemFault,
		},
		{
			name:         "Unrecognized failure fails open to recoverable",
			httpStatus:   400,
			msg:          "Unexpected format error",
			expectedCode: "UNKNOWN",
			expectedCls:  FailureRecoverable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, cls := ClassifyAadhaarError(tt.httpStatus, tt.msg)
			if code != tt.expectedCode {
				t.Errorf("expected code %s, got %s", tt.expectedCode, code)
			}
			if cls != tt.expectedCls {
				t.Errorf("expected class %s, got %s", tt.expectedCls, cls)
			}
		})
	}
}

func TestCooldownSecondsFor(t *testing.T) {
	if cd := CooldownSecondsFor(FailureRecoverable, "WRONG_OTP"); cd != 50 {
		t.Errorf("expected 50s cooldown for WRONG_OTP, got %d", cd)
	}
	if cd := CooldownSecondsFor(FailureRecoverable, "OTP_EXPIRED"); cd != 30 {
		t.Errorf("expected 30s cooldown for OTP_EXPIRED, got %d", cd)
	}
	if cd := CooldownSecondsFor(FailureCooldown, "COOLDOWN_ACTIVE"); cd != 45 {
		t.Errorf("expected 45s cooldown for COOLDOWN_ACTIVE, got %d", cd)
	}
	if cd := CooldownSecondsFor(FailureTerminal, "MOBILE_NOT_LINKED"); cd != 0 {
		t.Errorf("expected 0s cooldown for terminal, got %d", cd)
	}
	if cd := CooldownSecondsFor(FailureSystemFault, "PROVIDER_ERROR"); cd != 0 {
		t.Errorf("expected 0s cooldown for system fault, got %d", cd)
	}
}

func TestKYCSignature_RSAOAEP_SHA1(t *testing.T) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	fixedTime := time.Date(2026, 10, 30, 10, 0, 0, 0, time.UTC)
	clientID := "CF_SECURE_ID_CLIENT_99"

	var capturedSignature, capturedClientID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedSignature = r.Header.Get("X-Cf-Signature")
		capturedClientID = r.Header.Get("x-client-id")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ref_id":  "cf_ref_123",
			"status":  "SUCCESS",
			"message": "OTP sent",
		})
	}))
	defer srv.Close()

	client := &Client{
		cfg: Config{
			AppID:        clientID,
			SecretKey:    "test-secret",
			Env:          "sandbox",
			KYCAuthMode:  "signature",
			KYCPublicKey: &privKey.PublicKey,
			Now:          func() time.Time { return fixedTime },
		},
		http:         srv.Client(),
		baseOverride: srv.URL,
	}

	res, err := client.RequestAadhaarOTP(context.Background(), "999988887777")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.RefID != "cf_ref_123" {
		t.Errorf("expected ref_id cf_ref_123, got %s", res.RefID)
	}
	if capturedClientID != clientID {
		t.Errorf("expected x-client-id %s, got %s", clientID, capturedClientID)
	}
	if capturedSignature == "" {
		t.Fatal("expected X-Cf-Signature header to be set")
	}

	sigBytes, err := base64.StdEncoding.DecodeString(capturedSignature)
	if err != nil {
		t.Fatalf("signature is not valid base64: %v", err)
	}
	decryptedBytes, err := rsa.DecryptOAEP(sha1.New(), rand.Reader, privKey, sigBytes, nil)
	if err != nil {
		t.Fatalf("failed to decrypt signature: %v", err)
	}
	expected := fmt.Sprintf("%s.%d", clientID, fixedTime.Unix())
	if string(decryptedBytes) != expected {
		t.Errorf("decrypted signature mismatch: got %q, want %q", string(decryptedBytes), expected)
	}
}

func TestKYCSignature_FailClosedWhenSigningFails(t *testing.T) {
	serverReached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverReached = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := &Client{
		cfg: Config{
			AppID:        "CF_ID",
			SecretKey:    "test-secret",
			Env:          "sandbox",
			KYCAuthMode:  "signature",
			KYCPublicKey: nil, // missing public key in signature mode
		},
		http:         srv.Client(),
		baseOverride: srv.URL,
	}

	_, err := client.RequestAadhaarOTP(context.Background(), "999988887777")
	if err == nil {
		t.Fatal("expected error when kyc public key is nil in signature mode, got nil")
	}
	if serverReached {
		t.Fatal("expected request NOT to be dispatched when signature generation fails")
	}
}

func TestVerifyAadhaarOTP_PhotoLinkSupport(t *testing.T) {
	rawPhoto := []byte("fake-photo-binary-data")
	encodedPhoto := base64.StdEncoding.EncodeToString(rawPhoto)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ref_id":     "ref_abc_1",
			"status":     "VALID",
			"name":       "Ananya Sharma",
			"dob":        "2000-01-01",
			"gender":     "F",
			"uid":        "XXXXXXXX1234",
			"photo_link": "data:image/jpeg;base64," + encodedPhoto,
		})
	}))
	defer srv.Close()

	client := newKYCTestClient(srv)
	res, err := client.VerifyAadhaarOTP(context.Background(), "ref_abc_1", "123456")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Name != "Ananya Sharma" {
		t.Errorf("expected name Ananya Sharma, got %s", res.Name)
	}
	if string(res.PhotoBytes) != string(rawPhoto) {
		t.Errorf("expected photo bytes decoded from photo_link, got %s", string(res.PhotoBytes))
	}
	if !strings.Contains(res.PhotoLink, encodedPhoto) {
		t.Errorf("expected PhotoLink to be preserved, got %s", res.PhotoLink)
	}
}

