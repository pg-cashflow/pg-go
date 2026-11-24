package cashfree

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newDigiLockerServer(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return newKYCTestClient(srv), srv
}

func TestDigiLocker_NotConfigured(t *testing.T) {
	c := &Client{cfg: Config{}, http: http.DefaultClient}
	ctx := context.Background()
	if _, err := c.CreateDigiLockerURL(ctx, CreateDigiLockerURLInput{VerificationID: "v1", RedirectURL: "https://x.test"}); err == nil {
		t.Error("create: expected error when not configured")
	}
	if _, err := c.GetDigiLockerStatus(ctx, "v1"); err == nil {
		t.Error("status: expected error when not configured")
	}
	if _, err := c.GetDigiLockerAadhaar(ctx, "v1"); err == nil {
		t.Error("document: expected error when not configured")
	}
}

func TestCreateDigiLockerURL_Success(t *testing.T) {
	var gotBody map[string]any
	c, _ := newDigiLockerServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/digilocker" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-client-id") != "test-id" || r.Header.Get("x-client-secret") != "test-secret" {
			t.Error("credentials headers missing")
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"verification_id":"v1","reference_id":12345,
			"url":"https://verification-test.cashfree.com/dgl/h7562ci7us0","status":"PENDING",
			"user_flow":"signup","document_requested":["AADHAAR"]}`))
	})
	s, err := c.CreateDigiLockerURL(context.Background(), CreateDigiLockerURLInput{
		VerificationID: " v1 ", RedirectURL: "https://app.example.test/kyc/return", UserFlow: DigiLockerFlowSignUp,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if s.ReferenceID != "12345" || s.Status != DigiLockerPending || s.VerificationID != "v1" {
		t.Errorf("unexpected session %+v", s)
	}
	if gotBody["verification_id"] != "v1" {
		t.Errorf("verification_id not trimmed: %v", gotBody["verification_id"])
	}
	docs, _ := gotBody["document_requested"].([]any)
	if len(docs) != 1 || docs[0] != "AADHAAR" {
		t.Errorf("only AADHAAR may be requested, got %v", gotBody["document_requested"])
	}
	if gotBody["user_flow"] != "signup" || gotBody["redirect_url"] != "https://app.example.test/kyc/return" {
		t.Errorf("unexpected body %v", gotBody)
	}
}

func TestCreateDigiLockerURL_OmitsEmptyUserFlow(t *testing.T) {
	c, _ := newDigiLockerServer(t, func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		if _, present := m["user_flow"]; present {
			t.Error("user_flow must be omitted when empty")
		}
		_, _ = w.Write([]byte(`{"verification_id":"v1","reference_id":1,"url":"https://verification.cashfree.com/dgl/a","status":"PENDING"}`))
	})
	if _, err := c.CreateDigiLockerURL(context.Background(), CreateDigiLockerURLInput{VerificationID: "v1", RedirectURL: "https://a.test"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCreateDigiLockerURL_InputValidation(t *testing.T) {
	c, srv := newDigiLockerServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request may be sent for invalid input")
	})
	_ = srv
	cases := map[string]CreateDigiLockerURLInput{
		"missing verification id": {RedirectURL: "https://a.test"},
		"http redirect":           {VerificationID: "v1", RedirectURL: "http://a.test"},
		"empty redirect":          {VerificationID: "v1"},
		"bad user flow":           {VerificationID: "v1", RedirectURL: "https://a.test", UserFlow: "popup"},
	}
	for name, in := range cases {
		if _, err := c.CreateDigiLockerURL(context.Background(), in); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestCreateDigiLockerURL_RejectsUnsafeResponseURL(t *testing.T) {
	bad := []string{
		"",
		"http://verification.cashfree.com/dgl/a",
		"https://evil.example.com/dgl/a",
		"https://cashfree.com.evil.example/dgl/a",
		"https://notcashfree.com/dgl/a",
		"javascript:alert(1)",
	}
	for _, u := range bad {
		c, _ := newDigiLockerServer(t, func(w http.ResponseWriter, r *http.Request) {
			b, _ := json.Marshal(map[string]any{"verification_id": "v1", "reference_id": 1, "url": u, "status": "PENDING"})
			_, _ = w.Write(b)
		})
		_, err := c.CreateDigiLockerURL(context.Background(), CreateDigiLockerURLInput{VerificationID: "v1", RedirectURL: "https://a.test"})
		var corrupt *CorruptDocumentError
		if !errors.As(err, &corrupt) {
			t.Errorf("url %q: expected CorruptDocumentError, got %v", u, err)
		}
	}
}

func TestCreateDigiLockerURL_APIErrorCodes(t *testing.T) {
	c, _ := newDigiLockerServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"type":"validation_error","code":"redirect_url_value_invalid","message":"x"}`))
	})
	_, err := c.CreateDigiLockerURL(context.Background(), CreateDigiLockerURLInput{VerificationID: "v1", RedirectURL: "https://a.test"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || !apiErr.IsPermanent() {
		t.Fatalf("expected permanent APIError, got %v", err)
	}
	if got := DigiLockerErrorCode(err); got != "redirect_url_value_invalid" {
		t.Errorf("unexpected error code %q", got)
	}
}

func TestGetDigiLockerStatus(t *testing.T) {
	for _, want := range []DigiLockerStatus{DigiLockerPending, DigiLockerAuthenticated, DigiLockerExpired, DigiLockerConsentDenied} {
		c, _ := newDigiLockerServer(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || r.URL.Path != "/digilocker" || r.URL.Query().Get("verification_id") != "v 1&x=2" {
				t.Errorf("unexpected request %s %s", r.Method, r.URL.String())
			}
			_, _ = w.Write([]byte(`{"user_details":{},"status":"` + string(want) + `","verification_id":"v 1&x=2","reference_id":12345}`))
		})
		got, err := c.GetDigiLockerStatus(context.Background(), "v 1&x=2")
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", want, err)
		}
		if got.Status != want || got.ReferenceID != "12345" {
			t.Errorf("%s: unexpected result %+v", want, got)
		}
	}
	if DigiLockerPending.IsTerminal() {
		t.Error("PENDING must not be terminal")
	}
	for _, s := range []DigiLockerStatus{DigiLockerAuthenticated, DigiLockerExpired, DigiLockerConsentDenied} {
		if !s.IsTerminal() {
			t.Errorf("%s must be terminal", s)
		}
	}
	if DigiLockerStatus("SOMETHING_NEW").IsTerminal() {
		t.Error("unknown status must not be treated as terminal")
	}
}

func TestGetDigiLockerStatus_MissingStatusIsCorrupt(t *testing.T) {
	c, _ := newDigiLockerServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	_, err := c.GetDigiLockerStatus(context.Background(), "v1")
	var corrupt *CorruptDocumentError
	if !errors.As(err, &corrupt) {
		t.Fatalf("expected CorruptDocumentError, got %v", err)
	}
}

func TestGetDigiLockerAadhaar_Success(t *testing.T) {
	photo := base64.StdEncoding.EncodeToString([]byte("jpegbytes"))
	c, _ := newDigiLockerServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/digilocker/document/AADHAAR" || r.URL.Query().Get("verification_id") != "v1" {
			t.Errorf("unexpected request %s", r.URL.String())
		}
		_, _ = w.Write([]byte(`{"reference_id":408,"verification_id":"v1","status":"SUCCESS","uid":"xxxxxxxx5647",
			"care_of":"S/O: Someone","dob":"02-02-1995","gender":"M","name":"Mallesh Fakkirappa Dollin",
			"photo_link":"` + photo + `","split_address":{"pincode":"581115"},"year_of_birth":1995,
			"xml_file":"https://example.test/a.zip","message":"Aadhaar Card Exists"}`))
	})
	got, err := c.GetDigiLockerAadhaar(context.Background(), "v1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Status != DigiLockerDocSuccess || got.MaskedUID != "xxxxxxxx5647" || got.Name != "Mallesh Fakkirappa Dollin" ||
		got.DOB != "02-02-1995" || got.Gender != "M" || got.YearOfBirth != 1995 || string(got.PhotoBytes) != "jpegbytes" {
		t.Errorf("unexpected result %+v", got)
	}
}

func TestGetDigiLockerAadhaar_YearOnlyAndBadPhoto(t *testing.T) {
	c, _ := newDigiLockerServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"reference_id":1,"verification_id":"v1","status":"SUCCESS","uid":"xxxxxxxx1111",
			"name":"A B","gender":"F","year_of_birth":2001,"photo_link":"!!not-base64!!"}`))
	})
	got, err := c.GetDigiLockerAadhaar(context.Background(), "v1")
	if err != nil {
		t.Fatalf("a bad photo must not fail KYC: %v", err)
	}
	if got.DOB != "" || got.YearOfBirth != 2001 || got.PhotoBytes != nil {
		t.Errorf("unexpected result %+v", got)
	}
}

func TestGetDigiLockerAadhaar_NotLinkedIsNotAnError(t *testing.T) {
	c, _ := newDigiLockerServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"reference_id":408,"verification_id":"v1","status":"AADHAAR_NOT_LINKED","name":null,"dob":null,"uid":null}`))
	})
	got, err := c.GetDigiLockerAadhaar(context.Background(), "v1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Status != DigiLockerAadhaarNotLinked || got.Name != "" || got.MaskedUID != "" {
		t.Errorf("unexpected result %+v", got)
	}
}

func TestGetDigiLockerAadhaar_PendingIs202(t *testing.T) {
	c, _ := newDigiLockerServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"type":"validation_error","code":"validation_pending","message":"Validation in process check after some time"}`))
	})
	if _, err := c.GetDigiLockerAadhaar(context.Background(), "v1"); !errors.Is(err, ErrDigiLockerPending) {
		t.Fatalf("expected ErrDigiLockerPending, got %v", err)
	}
}

func TestGetDigiLockerAadhaar_IncompleteSuccessIsCorrupt(t *testing.T) {
	bodies := map[string]string{
		"no name":                     `{"status":"SUCCESS","uid":"xxxxxxxx5647","dob":"02-02-1995"}`,
		"no uid":                      `{"status":"SUCCESS","name":"A B","dob":"02-02-1995"}`,
		"no dob":                      `{"status":"SUCCESS","name":"A B","uid":"xxxxxxxx5647"}`,
		"unknown":                     `{"status":"WHATEVER","name":"A B","uid":"xxxxxxxx5647","dob":"02-02-1995"}`,
		"bad json":                    `not json`,
		"unmasked 12 digit uid":       `{"status":"SUCCESS","name":"A B","uid":"123456789012","dob":"02-02-1995"}`,
		"unmasked hyphenated aadhaar": `{"status":"SUCCESS","name":"A B","aadhaar_number":"1234-5678-9012","dob":"02-02-1995"}`,
		"short mask":                  `{"status":"SUCCESS","name":"A B","uid":"XXXX1234","dob":"02-02-1995"}`,
		"all mask without digits":     `{"status":"SUCCESS","name":"A B","uid":"XXXXXXXXXXXX","dob":"02-02-1995"}`,
	}
	for name, body := range bodies {
		c, _ := newDigiLockerServer(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
		_, err := c.GetDigiLockerAadhaar(context.Background(), "v1")
		var corrupt *CorruptDocumentError
		if !errors.As(err, &corrupt) {
			t.Errorf("%s: expected CorruptDocumentError, got %v", name, err)
		}
	}
}

func TestGetDigiLockerAadhaar_AadhaarNumberFallbackAndMasking(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantUID string
	}{
		{
			name:    "aadhaar_number used when uid missing",
			body:    `{"status":"SUCCESS","name":"John Doe","aadhaar_number":"XXXXXXXX5647","dob":"01-01-1990"}`,
			wantUID: "XXXXXXXX5647",
		},
		{
			name:    "hyphenated masked aadhaar accepted",
			body:    `{"status":"SUCCESS","name":"John Doe","aadhaar_number":"XXXX-XXXX-5647","dob":"01-01-1990"}`,
			wantUID: "XXXX-XXXX-5647",
		},
		{
			name:    "space-separated masked aadhaar accepted",
			body:    `{"status":"SUCCESS","name":"John Doe","uid":"XXXX XXXX 5647","dob":"01-01-1990"}`,
			wantUID: "XXXX XXXX 5647",
		},
		{
			name:    "asterisk-masked aadhaar accepted",
			body:    `{"status":"SUCCESS","name":"John Doe","uid":"********5647","dob":"01-01-1990"}`,
			wantUID: "********5647",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := newDigiLockerServer(t, func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			})
			got, err := c.GetDigiLockerAadhaar(context.Background(), "v1")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.MaskedUID != tc.wantUID {
				t.Errorf("got MaskedUID %q, want %q", got.MaskedUID, tc.wantUID)
			}
		})
	}
}

func TestGetDigiLockerAadhaar_ConsentErrorsArePermanentAndCoded(t *testing.T) {
	for _, code := range []string{"consent_not_granted", "url_expired", "session_expired"} {
		c, _ := newDigiLockerServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"validation_error","code":"` + code + `","message":"m"}`))
		})
		_, err := c.GetDigiLockerAadhaar(context.Background(), "v1")
		var apiErr *APIError
		if !errors.As(err, &apiErr) || !apiErr.IsPermanent() {
			t.Fatalf("%s: expected permanent APIError, got %v", code, err)
		}
		if got := DigiLockerErrorCode(err); got != code {
			t.Errorf("expected code %q, got %q", code, got)
		}
	}
}

func TestDigiLocker_TransientErrorsAreNotPermanent(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500, 502} {
		c, _ := newDigiLockerServer(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"code":"x"}`))
		})
		_, err := c.GetDigiLockerStatus(context.Background(), "v1")
		var apiErr *APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("%d: expected APIError, got %v", status, err)
		}
		if status < 500 && apiErr.IsPermanent() {
			t.Errorf("%d must not be permanent", status)
		}
	}
}

func TestDigiLocker_OversizeResponseRejected(t *testing.T) {
	c, _ := newDigiLockerServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"` + strings.Repeat("A", maxDigiLockerResponseBytes+10) + `"}`))
	})
	_, err := c.GetDigiLockerStatus(context.Background(), "v1")
	var corrupt *CorruptDocumentError
	if !errors.As(err, &corrupt) {
		t.Fatalf("expected CorruptDocumentError, got %v", err)
	}
}

func TestDigiLocker_RedirectsAreNotFollowed(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = true
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer srv.Close()
	c := newKYCTestClient(srv)
	c.http = newCashfreeHTTPClient(5e9)
	_, err := c.GetDigiLockerStatus(context.Background(), "v1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusFound {
		t.Fatalf("expected APIError 302, got %v", err)
	}
	if leaked {
		t.Error("credentials were forwarded to the redirect target")
	}
}

func TestDigiLocker_EmptyVerificationIDRejected(t *testing.T) {
	c, _ := newDigiLockerServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request may be sent for an empty verification id")
	})
	if _, err := c.GetDigiLockerStatus(context.Background(), "  "); err == nil {
		t.Error("status: expected error")
	}
	if _, err := c.GetDigiLockerAadhaar(context.Background(), ""); err == nil {
		t.Error("document: expected error")
	}
}
