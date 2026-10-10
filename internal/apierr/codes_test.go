package apierr

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/gin-gonic/gin"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func TestAllCodes_UniquenessAndFormat(t *testing.T) {
	codePattern := regexp.MustCompile(`^[a-z]+\.[a-zA-Z0-9]+$`)
	seen := make(map[Code]bool)

	for _, c := range AllCodes {
		s := string(c)
		if !codePattern.MatchString(s) {
			t.Errorf("code %q does not match expected pattern ^[a-z]+\\.[a-zA-Z0-9]+$", s)
		}
		if seen[c] {
			t.Errorf("duplicate error code found: %s", c)
		}
		seen[c] = true
	}
}

func TestErrorEnvelope_JSONSerialization(t *testing.T) {
	env := ErrorEnvelope{
		Error: "invalid credentials",
		Code:  CodeAuthForbidden,
	}

	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("failed to marshal ErrorEnvelope: %v", err)
	}

	var parsed map[string]any
	if err := json.Unmarshal(b, &parsed); err != nil {
		t.Fatalf("failed to unmarshal JSON: %v", err)
	}

	if parsed["error"] != "invalid credentials" {
		t.Errorf("expected error field %q, got %v", "invalid credentials", parsed["error"])
	}
	if parsed["code"] != string(CodeAuthForbidden) {
		t.Errorf("expected code field %q, got %v", CodeAuthForbidden, parsed["code"])
	}
}

func TestAbort(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	Abort(c, http.StatusUnauthorized, "token expired", CodeAuthOtpExpired)

	if !c.IsAborted() {
		t.Error("expected context to be aborted")
	}
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected status 401, got %d", w.Code)
	}

	var env ErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("failed to parse response JSON: %v", err)
	}
	if env.Error != "token expired" || env.Code != CodeAuthOtpExpired {
		t.Errorf("unexpected envelope content: %+v", env)
	}
}

func TestRespondBindErr(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	RespondBindErr(c, "invalid json body", CodeRequestInvalidBody)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected status 400, got %d", w.Code)
	}

	var env ErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("failed to parse response JSON: %v", err)
	}
	if env.Error != "invalid json body" || env.Code != CodeRequestInvalidBody {
		t.Errorf("unexpected envelope content: %+v", env)
	}
}

func TestRespondClientErr(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	RespondClientErr(c, http.StatusPaymentRequired, "overpay not allowed", CodeFinanceOverpay)

	if w.Code != http.StatusPaymentRequired {
		t.Errorf("expected status 402, got %d", w.Code)
	}

	var env ErrorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("failed to parse response JSON: %v", err)
	}
	if env.Error != "overpay not allowed" || env.Code != CodeFinanceOverpay {
		t.Errorf("unexpected envelope content: %+v", env)
	}
}
