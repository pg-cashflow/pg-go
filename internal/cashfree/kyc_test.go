package cashfree

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
