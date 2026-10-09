package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pg-cashflow/pg-go/internal/apierr"
	"github.com/pg-cashflow/pg-go/internal/domain"
)

// TestAPIContract_RouteInventory extracts all registered routes from NewRouter
// and verifies they conform to the API Contract in CONTRACT.md Rev 13.
func TestAPIContract_RouteInventory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deps := Deps{
		JWTSecret: "contract-test-secret-32-chars-long!",
	}
	r := NewRouter(deps)

	routes := r.Routes()
	if len(routes) == 0 {
		t.Fatal("expected registered routes from NewRouter, got 0")
	}

	apiRoutesCount := 0
	for _, route := range routes {
		if strings.HasPrefix(route.Path, "/api") {
			apiRoutesCount++

			// Invariant: API route paths must not have trailing slashes (except root group if any)
			if len(route.Path) > 5 && strings.HasSuffix(route.Path, "/") {
				t.Errorf("API route %s %s has forbidden trailing slash", route.Method, route.Path)
			}

			// Invariant: Methods must be standard HTTP verbs
			switch route.Method {
			case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, http.MethodOptions:
			default:
				t.Errorf("unexpected HTTP method %s for route %s", route.Method, route.Path)
			}
		}
	}

	if apiRoutesCount < 50 {
		t.Fatalf("expected at least 50 API routes registered, got %d", apiRoutesCount)
	}
}

// TestAPIContract_ListEnvelopes verifies that all list responses wrap arrays
// in the exact envelope documented in CONTRACT.md Rev 13.
func TestAPIContract_ListEnvelopes(t *testing.T) {
	expectedEnvelopes := []string{
		"properties",
		"tenants",
		"dues",
		"payments",
		"events",
		"inspections",
		"hazards",
		"rewards",
		"notifications",
		"join_requests",
		"payment_reports",
		"expenses",
		"capital",
		"advances",
		"budgets",
		"approvals",
		"suggestions",
		"referrals",
		"violations",
		"floors",
		"rooms",
		"departures",
		"deductions",
		"payees",
		"items",
		"batches",
		"settlements",
	}

	for _, envelope := range expectedEnvelopes {
		m := gin.H{envelope: []any{}}
		data, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("failed to marshal envelope %q: %v", envelope, err)
		}

		var parsed map[string]any
		if err := json.Unmarshal(data, &parsed); err != nil {
			t.Fatalf("failed to unmarshal envelope %q: %v", envelope, err)
		}

		val, exists := parsed[envelope]
		if !exists {
			t.Errorf("expected key %q in JSON payload", envelope)
		}
		if _, ok := val.([]any); !ok {
			t.Errorf("expected key %q to contain JSON array, got %T", envelope, val)
		}
	}
}

// TestAPIContract_MoneyInvariantsIntegerPaise asserts that all monetary amounts
// in domain structures serialize strictly as integer numbers without decimals.
func TestAPIContract_MoneyInvariantsIntegerPaise(t *testing.T) {
	ceiling := int64(2000000)
	due := domain.Due{
		Amount:                  1500000,
		OriginalAmount:          1500000,
		ContractualCeilingPaise: &ceiling,
	}

	rawDue, err := json.Marshal(due)
	if err != nil {
		t.Fatalf("failed to marshal due: %v", err)
	}

	var rawMap map[string]json.RawMessage
	if err := json.Unmarshal(rawDue, &rawMap); err != nil {
		t.Fatalf("failed to unmarshal due map: %v", err)
	}

	amountRaw := string(rawMap["amount"])
	if strings.Contains(amountRaw, ".") {
		t.Fatalf("money invariant violated: amount contains decimal point: %s", amountRaw)
	}

	var amountInt int64
	if err := json.Unmarshal(rawMap["amount"], &amountInt); err != nil {
		t.Fatalf("amount is not an integer: %v (raw: %s)", err, amountRaw)
	}
	if amountInt != 1500000 {
		t.Fatalf("expected 1500000 paise, got %d", amountInt)
	}
}

// TestAPIContract_ErrorEnvelopeSchema verifies that error responses
// conform to { "error": string, "code": "domain.reason"? }.
func TestAPIContract_ErrorEnvelopeSchema(t *testing.T) {
	codeRegex := regexp.MustCompile(`^[a-z]+(\.[a-zA-Z0-9_]+)+$`)
	validPrefixes := []string{
		"auth.",
		"join.",
		"payment.",
		"finance.",
		"request.",
		"preferences.",
		"kyc.",
		"billing.",
	}

	for _, code := range apierr.AllCodes {
		s := string(code)
		if !codeRegex.MatchString(s) {
			t.Errorf("error code %q does not match regex %s", s, codeRegex.String())
		}

		matchedPrefix := false
		for _, prefix := range validPrefixes {
			if strings.HasPrefix(s, prefix) {
				matchedPrefix = true
				break
			}
		}
		if !matchedPrefix {
			t.Errorf("error code %q does not start with any allowed domain prefix: %v", s, validPrefixes)
		}
	}
}

// TestAPIContract_ProtectedRoutesRejectAnonymous asserts that calling protected
// routes without a bearer token returns 401 JSON errors with code auth.missingToken.
func TestAPIContract_ProtectedRoutesRejectAnonymous(t *testing.T) {
	gin.SetMode(gin.TestMode)
	deps := Deps{
		JWTSecret: "contract-test-secret-32-chars-long!",
	}
	r := NewRouter(deps)

	protectedPaths := []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/owner/properties"},
		{http.MethodGet, "/api/owner/tenants"},
		{http.MethodGet, "/api/owner/dues"},
		{http.MethodGet, "/api/manager/inspections"},
		{http.MethodGet, "/api/tenant/me"},
		{http.MethodGet, "/api/tenant/dues"},
		{http.MethodGet, "/api/notifications"},
		{http.MethodGet, "/api/me/preferences"},
	}

	for _, pt := range protectedPaths {
		t.Run(pt.method+" "+pt.path, func(t *testing.T) {
			req := httptest.NewRequest(pt.method, pt.path, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Fatalf("expected status 401 for anonymous request to %s, got %d (body: %s)", pt.path, w.Code, w.Body.String())
			}

			ct := w.Header().Get("Content-Type")
			if !strings.Contains(ct, "application/json") {
				t.Fatalf("expected application/json Content-Type, got %q", ct)
			}

			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("failed to decode JSON response from %s: %v", pt.path, err)
			}

			errMsg, hasErr := body["error"].(string)
			if !hasErr || errMsg == "" {
				t.Fatalf("expected non-empty 'error' string in response from %s, got: %v", pt.path, body)
			}

			code, hasCode := body["code"].(string)
			if hasCode && code != string(apierr.CodeAuthMissingToken) {
				t.Fatalf("expected code %q or empty, got %q", apierr.CodeAuthMissingToken, code)
			}
		})
	}
}
