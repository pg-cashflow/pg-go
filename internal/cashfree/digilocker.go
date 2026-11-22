package cashfree

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// DigiLocker client for Cashfree Secure ID (Verification Suite).
//
// Flow (Cashfree "DigiLocker Integration" guide):
//  1. CreateDigiLockerURL  - POST /digilocker; the tenant opens the returned URL.
//  2. The tenant signs in (or signs up) to DigiLocker and gives consent.
//  3. GetDigiLockerStatus  - GET /digilocker; also delivered by webhook.
//  4. GetDigiLockerAadhaar - GET /digilocker/document/AADHAAR after AUTHENTICATED.
//
// Provider Architecture Note (KYC-CF-01):
// Cashfree documentation references an optional "Verify Account" pre-flight status
// check prior to URL generation. In this integration, merchant credentials are
// authenticated and authorized directly on the Verification Suite gateway during
// POST /digilocker via client credentials (x-client-id / x-client-secret). Omitting
// the extraneous synchronous account verification round-trip eliminates a redundant
// failure point without reducing security or authorization guarantees.
//
// Data minimisation: only the fields KYC needs are decoded. Address, care-of and the
// XML file link are never read, so they cannot be stored or logged by mistake.

const maxDigiLockerResponseBytes = 1 << 20 // 1 MiB

// DigiLockerStatus is the state of a DigiLocker consent session.
type DigiLockerStatus string

const (
	DigiLockerPending       DigiLockerStatus = "PENDING"
	DigiLockerAuthenticated DigiLockerStatus = "AUTHENTICATED"
	DigiLockerExpired       DigiLockerStatus = "EXPIRED"
	DigiLockerConsentDenied DigiLockerStatus = "CONSENT_DENIED"
)

// IsTerminal reports whether the session can no longer change state.
func (s DigiLockerStatus) IsTerminal() bool {
	switch s {
	case DigiLockerAuthenticated, DigiLockerExpired, DigiLockerConsentDenied:
		return true
	default:
		return false
	}
}

// Document-fetch status values (Get Document API).
const (
	DigiLockerDocSuccess       = "SUCCESS"
	DigiLockerAadhaarNotLinked = "AADHAAR_NOT_LINKED"
)

// DigiLocker user flows accepted by Create URL.
const (
	DigiLockerFlowSignIn = "signin"
	DigiLockerFlowSignUp = "signup"
)

// ErrDigiLockerPending means Cashfree has not finished preparing the document (HTTP 202).
// It is transient: retry later.
var ErrDigiLockerPending = errors.New("cashfree digilocker: document not ready yet")

// CreateDigiLockerURLInput is the request for CreateDigiLockerURL.
type CreateDigiLockerURLInput struct {
	// VerificationID is our own unique id for this session. Use the vendor ref id we store.
	VerificationID string
	// RedirectURL is where DigiLocker sends the tenant afterwards. Must be https.
	RedirectURL string
	// UserFlow is optional: "signin", "signup", or empty to let Cashfree decide.
	UserFlow string
}

// DigiLockerSession is the result of CreateDigiLockerURL.
type DigiLockerSession struct {
	VerificationID string
	ReferenceID    string
	URL            string
	Status         DigiLockerStatus
}

// DigiLockerStatusResult is the result of GetDigiLockerStatus.
type DigiLockerStatusResult struct {
	VerificationID string
	ReferenceID    string
	Status         DigiLockerStatus
}

// DigiLockerAadhaar holds the minimum Aadhaar fields KYC needs.
type DigiLockerAadhaar struct {
	VerificationID string
	ReferenceID    string
	// Status is DigiLockerDocSuccess or DigiLockerAadhaarNotLinked.
	Status    string
	MaskedUID string // for example "xxxxxxxx5647"
	Name      string
	DOB       string // Cashfree format DD-MM-YYYY; empty if only YearOfBirth is known
	Gender    string
	// YearOfBirth is set when the full date is not available.
	YearOfBirth int
	PhotoBytes  []byte
}

// CreateDigiLockerURL starts a DigiLocker consent session for an Aadhaar document.
func (c *Client) CreateDigiLockerURL(ctx context.Context, in CreateDigiLockerURLInput) (*DigiLockerSession, error) {
	if !c.cfg.Enabled() {
		return nil, fmt.Errorf("cashfree: not configured")
	}
	in.VerificationID = strings.TrimSpace(in.VerificationID)
	in.RedirectURL = strings.TrimSpace(in.RedirectURL)
	if in.VerificationID == "" {
		return nil, errors.New("cashfree digilocker: verification id is required")
	}
	if !strings.HasPrefix(in.RedirectURL, "https://") {
		return nil, errors.New("cashfree digilocker: redirect url must start with https://")
	}
	switch in.UserFlow {
	case "", DigiLockerFlowSignIn, DigiLockerFlowSignUp:
	default:
		return nil, fmt.Errorf("cashfree digilocker: unsupported user flow %q", in.UserFlow)
	}

	body, err := json.Marshal(struct {
		VerificationID    string   `json:"verification_id"`
		DocumentRequested []string `json:"document_requested"`
		RedirectURL       string   `json:"redirect_url"`
		UserFlow          string   `json:"user_flow,omitempty"`
	}{in.VerificationID, []string{"AADHAAR"}, in.RedirectURL, in.UserFlow})
	if err != nil {
		return nil, fmt.Errorf("cashfree digilocker: encode create url: %w", err)
	}

	raw, _, err := c.digiLockerDo(ctx, http.MethodPost, "/digilocker", nil, body)
	if err != nil {
		return nil, err
	}
	var out struct {
		VerificationID string     `json:"verification_id"`
		ReferenceID    ProviderID `json:"reference_id"`
		URL            string     `json:"url"`
		Status         string     `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &CorruptDocumentError{Reason: "digilocker create url: invalid json"}
	}
	if err := validateDigiLockerURL(out.URL); err != nil {
		return nil, err
	}
	return &DigiLockerSession{
		VerificationID: out.VerificationID,
		ReferenceID:    string(out.ReferenceID),
		URL:            out.URL,
		Status:         DigiLockerStatus(out.Status),
	}, nil
}

// validateDigiLockerURL fails closed: the URL is shown to the tenant's browser, so a tampered
// or malformed response must never be passed on. Cashfree serves it from *.cashfree.com.
func validateDigiLockerURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return &CorruptDocumentError{Reason: "digilocker create url: response url is not https"}
	}
	host := strings.ToLower(u.Hostname())
	if host != "cashfree.com" && !strings.HasSuffix(host, ".cashfree.com") {
		return &CorruptDocumentError{Reason: "digilocker create url: response host is not a cashfree domain"}
	}
	return nil
}

// GetDigiLockerStatus returns the current state of a consent session.
func (c *Client) GetDigiLockerStatus(ctx context.Context, verificationID string) (*DigiLockerStatusResult, error) {
	if !c.cfg.Enabled() {
		return nil, fmt.Errorf("cashfree: not configured")
	}
	verificationID = strings.TrimSpace(verificationID)
	if verificationID == "" {
		return nil, errors.New("cashfree digilocker: verification id is required")
	}
	raw, _, err := c.digiLockerDo(ctx, http.MethodGet, "/digilocker",
		url.Values{"verification_id": {verificationID}}, nil)
	if err != nil {
		return nil, err
	}
	var out struct {
		VerificationID string     `json:"verification_id"`
		ReferenceID    ProviderID `json:"reference_id"`
		Status         string     `json:"status"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &CorruptDocumentError{Reason: "digilocker status: invalid json"}
	}
	if out.Status == "" {
		return nil, &CorruptDocumentError{Reason: "digilocker status: missing status"}
	}
	return &DigiLockerStatusResult{
		VerificationID: out.VerificationID,
		ReferenceID:    string(out.ReferenceID),
		Status:         DigiLockerStatus(out.Status),
	}, nil
}

// GetDigiLockerAadhaar fetches the Aadhaar details after the session is AUTHENTICATED.
//
// It returns ErrDigiLockerPending for HTTP 202. A successful call with
// Status == DigiLockerAadhaarNotLinked is not an error: the tenant has no Aadhaar in DigiLocker.
func (c *Client) GetDigiLockerAadhaar(ctx context.Context, verificationID string) (*DigiLockerAadhaar, error) {
	if !c.cfg.Enabled() {
		return nil, fmt.Errorf("cashfree: not configured")
	}
	verificationID = strings.TrimSpace(verificationID)
	if verificationID == "" {
		return nil, errors.New("cashfree digilocker: verification id is required")
	}
	raw, status, err := c.digiLockerDo(ctx, http.MethodGet, "/digilocker/document/AADHAAR",
		url.Values{"verification_id": {verificationID}}, nil)
	if err != nil {
		return nil, err
	}
	if status == http.StatusAccepted {
		return nil, ErrDigiLockerPending
	}
	var out struct {
		VerificationID string     `json:"verification_id"`
		ReferenceID    ProviderID `json:"reference_id"`
		Status         string     `json:"status"`
		UID            string     `json:"uid"`
		Name           string     `json:"name"`
		DOB            string     `json:"dob"`
		Gender         string     `json:"gender"`
		YearOfBirth    int        `json:"year_of_birth"`
		PhotoLink      string     `json:"photo_link"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, &CorruptDocumentError{Reason: "digilocker aadhaar: invalid json"}
	}
	res := &DigiLockerAadhaar{
		VerificationID: out.VerificationID,
		ReferenceID:    string(out.ReferenceID),
		Status:         out.Status,
	}
	switch out.Status {
	case DigiLockerAadhaarNotLinked:
		return res, nil
	case DigiLockerDocSuccess:
	default:
		return nil, &CorruptDocumentError{Reason: "digilocker aadhaar: unexpected status " + out.Status}
	}
	// A "SUCCESS" without identity fields must never reach the verification pipeline.
	if strings.TrimSpace(out.Name) == "" || strings.TrimSpace(out.UID) == "" ||
		(strings.TrimSpace(out.DOB) == "" && out.YearOfBirth == 0) {
		return nil, &CorruptDocumentError{Reason: "digilocker aadhaar: success response is missing identity fields"}
	}
	res.MaskedUID = strings.TrimSpace(out.UID)
	res.Name = strings.TrimSpace(out.Name)
	res.DOB = strings.TrimSpace(out.DOB)
	res.Gender = strings.TrimSpace(out.Gender)
	res.YearOfBirth = out.YearOfBirth
	if out.PhotoLink != "" {
		// photo_link is a Base64 image. A bad photo must not block KYC; the photo is optional.
		if b, decErr := base64.StdEncoding.DecodeString(out.PhotoLink); decErr == nil {
			res.PhotoBytes = b
		}
	}
	return res, nil
}

// DigiLockerErrorCode extracts Cashfree's machine code (for example "consent_not_granted",
// "url_expired", "session_expired") from an APIError body. It returns "" for other errors.
func DigiLockerErrorCode(err error) string {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return ""
	}
	var body struct {
		Code string `json:"code"`
	}
	if json.Unmarshal([]byte(apiErr.Body), &body) != nil {
		return ""
	}
	return body.Code
}

// digiLockerDo sends one signed Secure ID request. It returns the body and status for 2xx
// (including 202) and an *APIError for everything else. The body size is capped.
func (c *Client) digiLockerDo(ctx context.Context, method, path string, query url.Values, body []byte) ([]byte, int, error) {
	endpoint := c.verificationEndpoint() + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
	if err != nil {
		return nil, 0, err
	}
	if err := c.signKYC(req); err != nil {
		return nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.kycClient().Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, readErr := io.ReadAll(io.LimitReader(resp.Body, maxDigiLockerResponseBytes+1))
	if readErr != nil {
		return nil, 0, fmt.Errorf("cashfree digilocker: read response body: %w", readErr)
	}
	if len(b) > maxDigiLockerResponseBytes {
		return nil, 0, &CorruptDocumentError{Reason: "digilocker response exceeds maximum size of 1 MiB"}
	}
	if resp.StatusCode >= 300 {
		return nil, resp.StatusCode, &APIError{StatusCode: resp.StatusCode, Status: resp.Status, Body: string(b)}
	}
	return b, resp.StatusCode, nil
}
