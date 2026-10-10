package cashfree

import (
	"context"
	"encoding/json"
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

func TestCreateDigiLockerLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/digilocker" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("x-client-id") != "test-id" {
			t.Errorf("missing x-client-id header")
		}

		var body struct {
			VerificationID string `json:"verification_id"`
			RedirectURL    string `json:"redirect_url"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.VerificationID == "" || body.RedirectURL == "" {
			t.Errorf("missing verification_id or redirect_url")
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"verification_url": "https://digilocker.gov.in/verify?session=abc",
		})
	}))
	defer srv.Close()

	c := newKYCTestClient(srv)
	url, err := c.CreateDigiLockerLink(context.Background(), "ver-001", "https://app.example.com/kyc/callback")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if url == "" {
		t.Error("expected non-empty verification URL")
	}
}

func TestCreateDigiLockerLink_NonSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"rate limited"}`, http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := newKYCTestClient(srv)
	_, err := c.CreateDigiLockerLink(context.Background(), "ver-002", "https://app.example.com/kyc/callback")
	if err == nil {
		t.Fatal("expected error from non-200 response")
	}
}

func TestGetDigiLockerStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/digilocker/ver-001" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "COMPLETED",
		})
	}))
	defer srv.Close()

	c := newKYCTestClient(srv)
	status, err := c.GetDigiLockerStatus(context.Background(), "ver-001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.Status != "COMPLETED" {
		t.Errorf("expected COMPLETED, got %s", status.Status)
	}
}

func TestGetDigiLockerDocument(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/digilocker/ver-001/document" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"aadhaar": map[string]string{
					"name":       "Ravi Kumar",
					"dob":        "1990-05-15",
					"gender":     "M",
					"masked_uid": "XXXX-XXXX-1234",
				},
			},
		})
	}))
	defer srv.Close()

	c := newKYCTestClient(srv)
	doc, err := c.GetDigiLockerDocument(context.Background(), "ver-001")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.Name != "Ravi Kumar" {
		t.Errorf("expected Ravi Kumar, got %s", doc.Name)
	}
	if doc.MaskedUID != "XXXX-XXXX-1234" {
		t.Errorf("unexpected masked_uid: %s", doc.MaskedUID)
	}
	if doc.DOB != "1990-05-15" {
		t.Errorf("unexpected dob: %s", doc.DOB)
	}
}

func TestGetDigiLockerDocument_IncompleteData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// name is present but masked_uid is missing — should error
		json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"aadhaar": map[string]string{
					"name":   "Ravi Kumar",
					"gender": "M",
				},
			},
		})
	}))
	defer srv.Close()

	c := newKYCTestClient(srv)
	_, err := c.GetDigiLockerDocument(context.Background(), "ver-001")
	if err == nil {
		t.Fatal("expected error for incomplete demographic data")
	}
}

func TestCashfreeKYCClient_NotConfigured(t *testing.T) {
	c := &Client{cfg: Config{}, http: http.DefaultClient}
	if _, err := c.CreateDigiLockerLink(context.Background(), "x", "y"); err == nil {
		t.Error("expected error when not configured")
	}
	if _, err := c.GetDigiLockerStatus(context.Background(), "x"); err == nil {
		t.Error("expected error when not configured")
	}
	if _, err := c.GetDigiLockerDocument(context.Background(), "x"); err == nil {
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

func TestGetDigiLockerDocument_4xxPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"verification not found"}`))
	}))
	defer srv.Close()

	c := newKYCTestClient(srv)
	_, err := c.GetDigiLockerDocument(context.Background(), "ver-404")
	if err == nil {
		t.Fatal("expected error")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if !apiErr.IsPermanent() {
		t.Errorf("expected 404 to be permanent")
	}
}
